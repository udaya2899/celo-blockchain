// Copyright 2017 The Celo Authors
// This file is part of the celo library.
//
// The celo library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The celo library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the celo library. If not, see <http://www.gnu.org/licenses/>.

package backend

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/istanbul"
	vet "github.com/ethereum/go-ethereum/consensus/istanbul/backend/internal/enodes"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/ecies"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

// ==============================================
//
// define the constants and function for the sendAnnounce thread

const (
	queryEnodeGossipCooldownDuration         = 5 * time.Minute
	versionCertificateGossipCooldownDuration = 5 * time.Minute
)

var (
	errInvalidEnodeCertMsgMap = errors.New("invalid enode certificate message map")
)

// QueryEnodeGossipFrequencyState specifies how frequently to gossip query enode messages
type QueryEnodeGossipFrequencyState int

const (
	// HighFreqBeforeFirstPeerState will send out a query enode message every 1 minute until the first peer is established
	HighFreqBeforeFirstPeerState QueryEnodeGossipFrequencyState = iota

	// HighFreqAfterFirstPeerState will send out an query enode message every 1 minute for the first 10 query enode messages after the first peer is established.
	// This is on the assumption that when this node first establishes a peer, the p2p network that this node is in may
	// be partitioned with the broader p2p network. We want to give that p2p network some time to connect to the broader p2p network.
	HighFreqAfterFirstPeerState

	// LowFreqState will send out an query every config.AnnounceQueryEnodeGossipPeriod seconds
	LowFreqState
)

// The announceThread will:
// 1) Periodically poll to see if this node should be announcing
// 2) Periodically share the entire version certificate table with all peers
// 3) Periodically prune announce-related data structures
// 4) Gossip announce messages periodically when requested
// 5) Update announce version when requested
func (sb *Backend) announceThread() {
	logger := sb.logger.New("func", "announceThread")

	sb.announceThreadWg.Add(1)
	defer sb.announceThreadWg.Done()

	// Create a ticker to poll if istanbul core is running and if this node is in
	// the validator conn set. If both conditions are true, then this node should announce.
	checkIfShouldAnnounceTicker := time.NewTicker(5 * time.Second)
	// Occasionally share the entire version certificate table with all peers
	shareVersionCertificatesTicker := time.NewTicker(5 * time.Minute)
	pruneAnnounceDataStructuresTicker := time.NewTicker(10 * time.Minute)

	var queryEnodeTicker *time.Ticker
	var queryEnodeTickerCh <-chan time.Time
	var queryEnodeFrequencyState QueryEnodeGossipFrequencyState
	var currentQueryEnodeTickerDuration time.Duration
	var numQueryEnodesInHighFreqAfterFirstPeerState int
	// TODO: this can be removed once we have more faith in this protocol
	var updateAnnounceVersionTicker *time.Ticker
	var updateAnnounceVersionTickerCh <-chan time.Time

	var announcing bool
	var shouldAnnounce bool
	var err error

	updateAnnounceVersionFunc := func() {
		version := getTimestamp()
		if version <= sb.GetAnnounceVersion() {
			logger.Debug("Announce version is not newer than the existing version", "existing version", sb.announceVersion, "attempted new version", version)
			return
		}
		if err := sb.setAndShareUpdatedAnnounceVersion(version); err != nil {
			logger.Warn("Error updating announce version", "err", err)
			return
		}
		sb.announceVersionMu.Lock()
		logger.Debug("Updating announce version", "announceVersion", version)
		sb.announceVersion = version
		sb.announceVersionMu.Unlock()
	}

	for {
		select {
		case <-checkIfShouldAnnounceTicker.C:
			logger.Trace("Checking if this node should announce it's enode")

			shouldAnnounce, err = sb.shouldSaveAndPublishValEnodeURLs()
			if err != nil {
				logger.Warn("Error in checking if should announce", err)
				break
			}

			if shouldAnnounce && !announcing {
				updateAnnounceVersionFunc()
				// Gossip the announce after a minute.
				// The delay allows for all receivers of the announce message to
				// have a more up-to-date cached registered/elected valset, and
				// hence more likely that they will be aware that this node is
				// within that set.
				waitPeriod := 1 * time.Minute
				if sb.config.Epoch <= 10 {
					waitPeriod = 5 * time.Second
				}
				time.AfterFunc(waitPeriod, func() {
					sb.startGossipQueryEnodeTask()
				})

				if sb.config.AnnounceAggressiveQueryEnodeGossipOnEnablement {
					queryEnodeFrequencyState = HighFreqBeforeFirstPeerState
					// Send an query enode message once a minute
					currentQueryEnodeTickerDuration = 1 * time.Minute
					numQueryEnodesInHighFreqAfterFirstPeerState = 0
				} else {
					queryEnodeFrequencyState = LowFreqState
					currentQueryEnodeTickerDuration = time.Duration(sb.config.AnnounceQueryEnodeGossipPeriod) * time.Second
				}

				// Enable periodic gossiping by setting announceGossipTickerCh to non nil value
				queryEnodeTicker = time.NewTicker(currentQueryEnodeTickerDuration)
				queryEnodeTickerCh = queryEnodeTicker.C

				updateAnnounceVersionTicker = time.NewTicker(5 * time.Minute)
				updateAnnounceVersionTickerCh = updateAnnounceVersionTicker.C

				announcing = true
				logger.Trace("Enabled periodic gossiping of announce message")
			} else if !shouldAnnounce && announcing {
				// Disable periodic queryEnode msgs by setting queryEnodeTickerCh to nil
				queryEnodeTicker.Stop()
				queryEnodeTickerCh = nil
				// Disable periodic updating of announce version
				updateAnnounceVersionTicker.Stop()
				updateAnnounceVersionTickerCh = nil

				announcing = false
				logger.Trace("Disabled periodic gossiping of announce message")
			}

		case <-shareVersionCertificatesTicker.C:
			// Send all version certificates to every peer. Only the entries
			// that are new to a node will end up being regossiped throughout the
			// network.
			allVersionCertificates, err := sb.getAllVersionCertificates()
			if err != nil {
				logger.Warn("Error getting all version certificates", "err", err)
				break
			}
			if err := sb.gossipVersionCertificatesMsg(allVersionCertificates); err != nil {
				logger.Warn("Error gossiping all version certificates")
			}

		case <-updateAnnounceVersionTickerCh:
			updateAnnounceVersionFunc()

		case <-queryEnodeTickerCh:
			sb.startGossipQueryEnodeTask()

		case <-sb.generateAndGossipQueryEnodeCh:
			if shouldAnnounce {
				switch queryEnodeFrequencyState {
				case HighFreqBeforeFirstPeerState:
					if len(sb.broadcaster.FindPeers(nil, p2p.AnyPurpose)) > 0 {
						queryEnodeFrequencyState = HighFreqAfterFirstPeerState
					}

				case HighFreqAfterFirstPeerState:
					if numQueryEnodesInHighFreqAfterFirstPeerState >= 10 {
						queryEnodeFrequencyState = LowFreqState
					}
					numQueryEnodesInHighFreqAfterFirstPeerState++

				case LowFreqState:
					if currentQueryEnodeTickerDuration != time.Duration(sb.config.AnnounceQueryEnodeGossipPeriod)*time.Second {
						// Reset the ticker
						currentQueryEnodeTickerDuration = time.Duration(sb.config.AnnounceQueryEnodeGossipPeriod) * time.Second
						queryEnodeTicker.Stop()
						queryEnodeTicker = time.NewTicker(currentQueryEnodeTickerDuration)
						queryEnodeTickerCh = queryEnodeTicker.C
					}
				}
				// This node may have recently sent out an announce message within
				// the gossip cooldown period imposed by other nodes.
				// Regardless, send the queryEnode so that it will at least be
				// processed by this node's peers. This is especially helpful when a network
				// is first starting up.
				if err := sb.generateAndGossipQueryEnode(sb.GetAnnounceVersion(), queryEnodeFrequencyState == LowFreqState); err != nil {
					logger.Warn("Error in generating and gossiping queryEnode", "err", err)
				}
			}

		case <-sb.updateAnnounceVersionCh:
			// Drain this channel, as the update version action will address all requests.

		drainLoop:
			for {
				select {
				case <-sb.updateAnnounceVersionCh:
				default:
					break drainLoop
				}
			}

			updateAnnounceVersionFunc()

		case <-pruneAnnounceDataStructuresTicker.C:
			if err := sb.pruneAnnounceDataStructures(); err != nil {
				logger.Warn("Error in pruning announce data structures", "err", err)
			}

		case <-sb.announceThreadQuit:
			checkIfShouldAnnounceTicker.Stop()
			pruneAnnounceDataStructuresTicker.Stop()
			if announcing {
				queryEnodeTicker.Stop()
				updateAnnounceVersionTicker.Stop()
			}
			return
		}
	}
}

// startGossipQueryEnodeTask will schedule a task for the announceThread to
// generate and gossip a queryEnode message
func (sb *Backend) startGossipQueryEnodeTask() {
	// sb.generateAndGossipQueryEnodeCh has a buffer of 1. If there is a value
	// already sent to the channel that has not been read from, don't block.
	select {
	case sb.generateAndGossipQueryEnodeCh <- struct{}{}:
	default:
	}
}

func (sb *Backend) shouldSaveAndPublishValEnodeURLs() (bool, error) {

	// Check if this node is in the validator connection set
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		return false, err
	}

	return validatorConnSet[sb.Address()], nil
}

// pruneAnnounceDataStructures will remove entries that are not in the validator connection set from all announce related data structures.
// The data structures that it prunes are:
// 1)  lastQueryEnodeGossiped
// 2)  valEnodeTable
// 3)  lastVersionCertificatesGossiped
// 4)  versionCertificateTable
func (sb *Backend) pruneAnnounceDataStructures() error {
	logger := sb.logger.New("func", "pruneAnnounceDataStructures")

	// retrieve the validator connection set
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		return err
	}

	sb.lastQueryEnodeGossipedMu.Lock()
	for remoteAddress := range sb.lastQueryEnodeGossiped {
		if !validatorConnSet[remoteAddress] && time.Since(sb.lastQueryEnodeGossiped[remoteAddress]) >= queryEnodeGossipCooldownDuration {
			logger.Trace("Deleting entry from lastQueryEnodeGossiped", "address", remoteAddress, "gossip timestamp", sb.lastQueryEnodeGossiped[remoteAddress])
			delete(sb.lastQueryEnodeGossiped, remoteAddress)
		}
	}
	sb.lastQueryEnodeGossipedMu.Unlock()

	if err := sb.valEnodeTable.PruneEntries(validatorConnSet); err != nil {
		logger.Trace("Error in pruning valEnodeTable", "err", err)
		return err
	}

	sb.lastVersionCertificatesGossipedMu.Lock()
	for remoteAddress := range sb.lastVersionCertificatesGossiped {
		if !validatorConnSet[remoteAddress] && time.Since(sb.lastVersionCertificatesGossiped[remoteAddress]) >= versionCertificateGossipCooldownDuration {
			logger.Trace("Deleting entry from lastVersionCertificatesGossiped", "address", remoteAddress, "gossip timestamp", sb.lastVersionCertificatesGossiped[remoteAddress])
			delete(sb.lastVersionCertificatesGossiped, remoteAddress)
		}
	}
	sb.lastVersionCertificatesGossipedMu.Unlock()

	if err := sb.versionCertificateTable.Prune(validatorConnSet); err != nil {
		logger.Trace("Error in pruning versionCertificateTable", "err", err)
		return err
	}

	return nil
}

// ===============================================================
//
// define the IstanbulQueryEnode message format, the QueryEnodeMsgCache entries, the queryEnode send function (both the gossip version and the "retrieve from cache" version), and the announce get function

type encryptedEnodeURL struct {
	DestAddress       common.Address
	EncryptedEnodeURL []byte
}

func (ee *encryptedEnodeURL) String() string {
	return fmt.Sprintf("{DestAddress: %s, EncryptedEnodeURL length: %d}", ee.DestAddress.String(), len(ee.EncryptedEnodeURL))
}

type queryEnodeData struct {
	EncryptedEnodeURLs []*encryptedEnodeURL
	Version            uint
	// The timestamp of the node when the message is generated.
	// This results in a new hash for a newly generated message so it gets regossiped by other nodes
	Timestamp uint
}

func (qed *queryEnodeData) String() string {
	return fmt.Sprintf("{Version: %v, Timestamp: %v, EncryptedEnodeURLs: %v}", qed.Version, qed.Timestamp, qed.EncryptedEnodeURLs)
}

// ==============================================
//
// define the functions that needs to be provided for rlp Encoder/Decoder.

// EncodeRLP serializes ar into the Ethereum RLP format.
func (ee *encryptedEnodeURL) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{ee.DestAddress, ee.EncryptedEnodeURL})
}

// DecodeRLP implements rlp.Decoder, and load the ar fields from a RLP stream.
func (ee *encryptedEnodeURL) DecodeRLP(s *rlp.Stream) error {
	var msg struct {
		DestAddress       common.Address
		EncryptedEnodeURL []byte
	}

	if err := s.Decode(&msg); err != nil {
		return err
	}
	ee.DestAddress, ee.EncryptedEnodeURL = msg.DestAddress, msg.EncryptedEnodeURL
	return nil
}

// EncodeRLP serializes ad into the Ethereum RLP format.
func (qed *queryEnodeData) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{qed.EncryptedEnodeURLs, qed.Version, qed.Timestamp})
}

// DecodeRLP implements rlp.Decoder, and load the ad fields from a RLP stream.
func (qed *queryEnodeData) DecodeRLP(s *rlp.Stream) error {
	var msg struct {
		EncryptedEnodeURLs []*encryptedEnodeURL
		Version            uint
		Timestamp          uint
	}

	if err := s.Decode(&msg); err != nil {
		return err
	}
	qed.EncryptedEnodeURLs, qed.Version, qed.Timestamp = msg.EncryptedEnodeURLs, msg.Version, msg.Timestamp
	return nil
}

// generateAndGossipAnnounce will generate the lastest announce msg from this node
// and then broadcast it to it's peers, which should then gossip the announce msg
// message throughout the p2p network if there has not been a message sent from
// this node within the last announceGossipCooldownDuration.
// Note that this function must ONLY be called by the announceThread.
func (sb *Backend) generateAndGossipQueryEnode(version uint, enforceRetryBackoff bool) error {
	logger := sb.logger.New("func", "generateAndGossipQueryEnode")
	logger.Trace("generateAndGossipQueryEnode called")

	// Retrieve the set valEnodeEntries (and their publicKeys)
	// for the queryEnode message
	valEnodeEntries, err := sb.getQueryEnodeValEnodeEntries(enforceRetryBackoff)
	if err != nil {
		return err
	}

	var valProxyAssignments map[common.Address]*enode.Node
	var selfEnodeURL string
	if sb.IsProxiedValidator() {
		valProxyAssignments, err = sb.proxyEngine.GetValidatorProxyAssignments()
		if err != nil {
			return err
		}
	} else {
		selfEnodeURL = sb.SelfNode().URLv4()
	}

	var queryEnodeEncryptedEnodeURLParams []*genEncryptedEnodeURLParam
	for _, valEnodeEntry := range valEnodeEntries {
		if valEnodeEntry.PublicKey != nil {
			var queryEnodeExternalEnodeURL string
			if sb.IsProxiedValidator() {
				if proxyNode, ok := valProxyAssignments[valEnodeEntry.Address]; ok && proxyNode != nil {
					queryEnodeExternalEnodeURL = proxyNode.URLv4()
				} else {
					continue
				}
			} else { // For the standalone validator case
				queryEnodeExternalEnodeURL = selfEnodeURL
			}

			queryEnodeEncryptedEnodeURLParams = append(queryEnodeEncryptedEnodeURLParams, &genEncryptedEnodeURLParam{destAddress: valEnodeEntry.Address,
				publicKey: valEnodeEntry.PublicKey,
				enodeURL:  queryEnodeExternalEnodeURL})
		}
	}

	if len(queryEnodeEncryptedEnodeURLParams) > 0 {
		istMsg, err := sb.generateQueryEnodeMsg(version, queryEnodeEncryptedEnodeURLParams)
		if err != nil {
			return err
		}

		if istMsg == nil {
			return nil
		}

		// Convert to payload
		payload, err := istMsg.Payload()
		if err != nil {
			logger.Error("Error in converting Istanbul QueryEnode Message to payload", "QueryEnodeMsg", istMsg.String(), "err", err)
			return err
		}

		if err := sb.Gossip(payload, istanbul.QueryEnodeMsg); err != nil {
			return err
		}

		if err := sb.valEnodeTable.UpdateQueryEnodeStats(valEnodeEntries); err != nil {
			return err
		}
	}

	return err
}

func (sb *Backend) getQueryEnodeValEnodeEntries(enforceRetryBackoff bool) ([]*istanbul.AddressEntry, error) {
	logger := sb.logger.New("func", "getQueryEnodeValEnodeEntries")
	valEnodeEntries, err := sb.valEnodeTable.GetValEnodes(nil)
	if err != nil {
		return nil, err
	}

	var queryEnodeValEnodeEntries []*istanbul.AddressEntry
	for address, valEnodeEntry := range valEnodeEntries {
		// Don't generate an announce record for ourselves
		if address == sb.Address() {
			continue
		}

		if valEnodeEntry.Version == valEnodeEntry.HighestKnownVersion {
			continue
		}

		if valEnodeEntry.PublicKey == nil {
			logger.Warn("Cannot generate encrypted enode URL for a val enode entry without a PublicKey", "address", address)
			continue
		}

		if enforceRetryBackoff && valEnodeEntry.NumQueryAttemptsForHKVersion > 0 {
			timeoutFactorPow := math.Min(float64(valEnodeEntry.NumQueryAttemptsForHKVersion-1), 5)
			timeoutMinutes := int64(math.Pow(1.5, timeoutFactorPow) * 5)
			timeoutForQuery := time.Duration(timeoutMinutes) * time.Minute

			if time.Since(*valEnodeEntry.LastQueryTimestamp) < timeoutForQuery {
				continue
			}
		}

		queryEnodeValEnodeEntries = append(queryEnodeValEnodeEntries, valEnodeEntry)
	}

	return queryEnodeValEnodeEntries, nil
}

// generateQueryEnodeMsg returns a queryEnode message from this node with a given version.
func (sb *Backend) generateQueryEnodeMsg(version uint, queryEnodeEncryptedEnodeURLParams []*genEncryptedEnodeURLParam) (*istanbul.Message, error) {
	logger := sb.logger.New("func", "generateQueryEnodeMsg")

	encryptedEnodeURLs, err := sb.generateEncryptedEnodeURLs(queryEnodeEncryptedEnodeURLParams)
	if err != nil {
		logger.Warn("Error generating encrypted enodeURLs", "err", err)
		return nil, err
	}
	if len(encryptedEnodeURLs) == 0 {
		logger.Trace("No encrypted enodeURLs were generated, will not generate encryptedEnodeMsg")
		return nil, nil
	}
	queryEnodeData := &queryEnodeData{
		EncryptedEnodeURLs: encryptedEnodeURLs,
		Version:            version,
		Timestamp:          getTimestamp(),
	}

	queryEnodeBytes, err := rlp.EncodeToBytes(queryEnodeData)
	if err != nil {
		logger.Error("Error encoding queryEnode content", "QueryEnodeData", queryEnodeData.String(), "err", err)
		return nil, err
	}

	msg := &istanbul.Message{
		Code:      istanbul.QueryEnodeMsg,
		Msg:       queryEnodeBytes,
		Address:   sb.Address(),
		Signature: []byte{},
	}

	// Sign the announce message
	if err := msg.Sign(sb.Sign); err != nil {
		logger.Error("Error in signing a QueryEnode Message", "QueryEnodeMsg", msg.String(), "err", err)
		return nil, err
	}

	logger.Debug("Generated a queryEnode message", "IstanbulMsg", msg.String(), "QueryEnodeData", queryEnodeData.String())

	return msg, nil
}

type genEncryptedEnodeURLParam struct {
	destAddress common.Address
	publicKey   *ecdsa.PublicKey
	enodeURL    string
}

// generateEncryptedEnodeURLs returns the encryptedEnodeURLs intended for validators
// whose entries in the val enode table do not exist or are outdated when compared
// to the version certificate table.
func (sb *Backend) generateEncryptedEnodeURLs(queryEnodeEncryptedEnodeURLParams []*genEncryptedEnodeURLParam) ([]*encryptedEnodeURL, error) {
	logger := sb.logger.New("func", "generateEncryptedEnodeURLs")

	var encryptedEnodeURLs []*encryptedEnodeURL
	for _, param := range queryEnodeEncryptedEnodeURLParams {
		logger.Info("encrypting enodeURL", "externalEnodeURL", param.enodeURL, "publicKey", param.publicKey)
		publicKey := ecies.ImportECDSAPublic(param.publicKey)
		encEnodeURL, err := ecies.Encrypt(rand.Reader, publicKey, []byte(param.enodeURL), nil, nil)
		if err != nil {
			logger.Error("Error in encrypting enodeURL", "enodeURL", param.enodeURL, "publicKey", publicKey)
			return nil, err
		}

		encryptedEnodeURLs = append(encryptedEnodeURLs, &encryptedEnodeURL{
			DestAddress:       param.destAddress,
			EncryptedEnodeURL: encEnodeURL,
		})
	}

	return encryptedEnodeURLs, nil
}

// This function will handle a queryEnode message.
func (sb *Backend) handleQueryEnodeMsg(addr common.Address, peer consensus.Peer, payload []byte) error {
	logger := sb.logger.New("func", "handleQueryEnodeMsg")

	// Since this is a gossiped messaged, mark that the peer gossiped it and check to see if this node already gossiped it
	sb.markMessageProcessedByPeer(addr, payload)
	if sb.checkIfMessageProcessedBySelf(payload) {
		return nil
	}
	defer sb.markMessageProcessedBySelf(payload)

	msg := new(istanbul.Message)

	// Since this is a gossiped messaged, mark that the peer gossiped it (and presumably processed it) and check to see if this node already processed it
	sb.markMessageProcessedByPeer(addr, payload)
	if sb.checkIfMessageProcessedBySelf(payload) {
		return nil
	}
	defer sb.markMessageProcessedBySelf(payload)

	// Decode message
	err := msg.FromPayload(payload, istanbul.GetSignatureAddress)
	if err != nil {
		logger.Error("Error in decoding received Istanbul Announce message", "err", err, "payload", hex.EncodeToString(payload))
		return err
	}
	logger.Trace("Handling an IstanbulAnnounce message", "from", msg.Address)

	// Check if the sender is within the validator connection set
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		logger.Trace("Error in retrieving validator connection set", "err", err)
		return err
	}

	if !validatorConnSet[msg.Address] {
		logger.Debug("Received a message from a validator not within the validator connection set. Ignoring it.", "sender", msg.Address)
		return errUnauthorizedAnnounceMessage
	}

	var qeData queryEnodeData
	err = rlp.DecodeBytes(msg.Msg, &qeData)
	if err != nil {
		logger.Warn("Error in decoding received Istanbul QueryEnode message content", "err", err, "IstanbulMsg", msg.String())
		return err
	}

	logger = logger.New("msgAddress", msg.Address, "msgVersion", qeData.Version)

	// Do some validation checks on the queryEnodeData
	if isValid, err := sb.validateQueryEnode(msg.Address, &qeData); !isValid || err != nil {
		logger.Warn("Validation of queryEnode message failed", "isValid", isValid, "err", err)
		return err
	}

	// If this is an elected or nearly elected validator and core is started, then process the queryEnode message
	shouldProcess, err := sb.shouldSaveAndPublishValEnodeURLs()
	if err != nil {
		logger.Warn("Error in checking if should process queryEnode", err)
	}

	if shouldProcess {
		logger.Trace("Processing an queryEnode message", "queryEnode records", qeData.EncryptedEnodeURLs)
		for _, encEnodeURL := range qeData.EncryptedEnodeURLs {
			// Only process an encEnodURL intended for this node
			if encEnodeURL.DestAddress != sb.Address() {
				continue
			}
			enodeBytes, err := sb.decryptFn(accounts.Account{Address: sb.Address()}, encEnodeURL.EncryptedEnodeURL, nil, nil)
			if err != nil {
				sb.logger.Warn("Error decrypting endpoint", "err", err, "encEnodeURL.EncryptedEnodeURL", encEnodeURL.EncryptedEnodeURL)
				return err
			}
			enodeURL := string(enodeBytes)
			node, err := enode.ParseV4(enodeURL)
			if err != nil {
				logger.Warn("Error parsing enodeURL", "enodeUrl", enodeURL)
				return err
			}

			// queryEnode messages should only be processed once because selfRecentMessages
			// will cache seen queryEnode messages, so it's safe to answer without any throttling
			if err := sb.answerQueryEnodeMsg(msg.Address, node, qeData.Version); err != nil {
				logger.Warn("Error answering an announce msg", "target node", node.URLv4(), "error", err)
				return err
			}

			break
		}
	}

	// Regossip this queryEnode message
	return sb.regossipQueryEnode(msg, qeData.Version, payload)
}

// answerQueryEnodeMsg will answer a received queryEnode message from an origin
// node. If the origin node is already a peer of any kind, an enodeCertificate will be sent.
// Regardless, the origin node will be upserted into the val enode table
// to ensure this node designates the origin node as a ValidatorPurpose peer.
func (sb *Backend) answerQueryEnodeMsg(address common.Address, node *enode.Node, version uint) error {
	targetIDs := map[enode.ID]bool{
		node.ID(): true,
	}
	// The target could be an existing peer of any purpose.
	matches := sb.broadcaster.FindPeers(targetIDs, p2p.AnyPurpose)
	if matches[node.ID()] != nil {
		enodeCertificateMsgMap := sb.RetrieveEnodeCertificateMsgMap()

		if enodeCertMsg := enodeCertificateMsgMap[sb.SelfNode().ID()]; enodeCertMsg != nil {
			if err := sb.sendEnodeCertificateMsg(matches[node.ID()], enodeCertMsg); err != nil {
				return err
			}
		}
	}
	// Upsert regardless to account for the case that the target is a non-ValidatorPurpose
	// peer but should be.
	// If the target is not a peer and should be a ValidatorPurpose peer, this
	// will designate the target as a ValidatorPurpose peer and send an enodeCertificate
	// during the istanbul handshake.
	if err := sb.valEnodeTable.UpsertVersionAndEnode([]*istanbul.AddressEntry{{Address: address, Node: node, Version: version}}); err != nil {
		return err
	}
	return nil
}

// validateQueryEnode will do some validation to check the contents of the queryEnode
// message. This is to force all validators that send a queryEnode message to
// create as succint message as possible, and prevent any possible network DOS attacks
// via extremely large queryEnode message.
func (sb *Backend) validateQueryEnode(msgAddress common.Address, qeData *queryEnodeData) (bool, error) {
	logger := sb.logger.New("func", "validateQueryEnode", "msg address", msgAddress)

	// Check if there are any duplicates in the queryEnode message
	var encounteredAddresses = make(map[common.Address]bool)
	for _, encEnodeURL := range qeData.EncryptedEnodeURLs {
		if encounteredAddresses[encEnodeURL.DestAddress] {
			logger.Info("QueryEnode message has duplicate entries", "address", encEnodeURL.DestAddress)
			return false, nil
		}

		encounteredAddresses[encEnodeURL.DestAddress] = true
	}

	// Check if the number of rows in the queryEnodePayload is at most 2 times the size of the current validator connection set.
	// Note that this is a heuristic of the actual size of validator connection set at the time the validator constructed the announce message.
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		return false, err
	}

	if len(qeData.EncryptedEnodeURLs) > 2*len(validatorConnSet) {
		logger.Info("Number of queryEnode message encrypted enodes is more than two times the size of the current validator connection set", "num queryEnode enodes", len(qeData.EncryptedEnodeURLs), "reg/elected val set size", len(validatorConnSet))
		return false, err
	}

	return true, nil
}

// regossipQueryEnode will regossip a received querEnode message.
// If this node regossiped a queryEnode from the same source address within the last
// 5 minutes, then it won't regossip. This is to prevent a malicious validator from
// DOS'ing the network with very frequent announce messages.
// This opens an attack vector where any malicious node could continue to gossip
// a previously gossiped announce message from any validator, causing other nodes to regossip and
// enforce the cooldown period for future messages originating from the origin validator.
// This is circumvented by caching the hashes of messages that are regossiped
// with sb.selfRecentMessages to prevent future regossips.
func (sb *Backend) regossipQueryEnode(msg *istanbul.Message, msgTimestamp uint, payload []byte) error {
	logger := sb.logger.New("func", "regossipQueryEnode", "queryEnodeSourceAddress", msg.Address, "msgTimestamp", msgTimestamp)
	sb.lastQueryEnodeGossipedMu.Lock()
	defer sb.lastQueryEnodeGossipedMu.Unlock()

	// Don't throttle messages from our own address so that proxies always regossip
	// query enode messages sent from the proxied validator
	if msg.Address != sb.ValidatorAddress() {
		if lastGossiped, ok := sb.lastQueryEnodeGossiped[msg.Address]; ok {
			if time.Since(lastGossiped) < queryEnodeGossipCooldownDuration {
				logger.Trace("Already regossiped msg from this source address within the cooldown period, not regossiping.")
				return nil
			}
		}
	}

	logger.Trace("Regossiping the istanbul queryEnode message", "IstanbulMsg", msg.String())
	if err := sb.Gossip(payload, istanbul.QueryEnodeMsg); err != nil {
		return err
	}

	sb.lastQueryEnodeGossiped[msg.Address] = time.Now()

	return nil
}

// Used as a salt when signing versionCertificate. This is to account for
// the unlikely case where a different signed struct with the same field types
// is used elsewhere and shared with other nodes. If that were to happen, a
// malicious node could try sending the other struct where this struct is used,
// or vice versa. This ensures that the signature is only valid for this struct.
var versionCertificateSalt = []byte("versionCertificate")

// versionCertificate is a signed message from a validator indicating the most
// recent version of its enode.
type versionCertificate vet.VersionCertificateEntry

func newVersionCertificateFromEntry(entry *vet.VersionCertificateEntry) *versionCertificate {
	return &versionCertificate{
		Address:   entry.Address,
		PublicKey: entry.PublicKey,
		Version:   entry.Version,
		Signature: entry.Signature,
	}
}

func (vc *versionCertificate) Sign(signingFn func(data []byte) ([]byte, error)) error {
	payloadToSign, err := vc.payloadToSign()
	if err != nil {
		return err
	}
	vc.Signature, err = signingFn(payloadToSign)
	if err != nil {
		return err
	}
	return nil
}

// RecoverPublicKeyAndAddress recovers the ECDSA public key and corresponding
// address from the Signature
func (vc *versionCertificate) RecoverPublicKeyAndAddress() error {
	payloadToSign, err := vc.payloadToSign()
	if err != nil {
		return err
	}
	payloadHash := crypto.Keccak256(payloadToSign)
	publicKey, err := crypto.SigToPub(payloadHash, vc.Signature)
	if err != nil {
		return err
	}
	address, err := crypto.PubkeyToAddress(*publicKey), nil
	if err != nil {
		return err
	}
	vc.PublicKey = publicKey
	vc.Address = address
	return nil
}

// EncodeRLP serializes versionCertificate into the Ethereum RLP format.
// Only the Version and Signature are encoded, as the public key and address
// can be recovered from the Signature using RecoverPublicKeyAndAddress
func (vc *versionCertificate) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{vc.Version, vc.Signature})
}

// DecodeRLP implements rlp.Decoder, and load the versionCertificate fields from a RLP stream.
// Only the Version and Signature are encoded/decoded, as the public key and address
// can be recovered from the Signature using RecoverPublicKeyAndAddress
func (vc *versionCertificate) DecodeRLP(s *rlp.Stream) error {
	var msg struct {
		Version   uint
		Signature []byte
	}

	if err := s.Decode(&msg); err != nil {
		return err
	}
	vc.Version, vc.Signature = msg.Version, msg.Signature
	return nil
}

func (vc *versionCertificate) Entry() *vet.VersionCertificateEntry {
	return &vet.VersionCertificateEntry{
		Address:   vc.Address,
		PublicKey: vc.PublicKey,
		Version:   vc.Version,
		Signature: vc.Signature,
	}
}

func (vc *versionCertificate) payloadToSign() ([]byte, error) {
	signedContent := []interface{}{versionCertificateSalt, vc.Version}
	payload, err := rlp.EncodeToBytes(signedContent)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func (sb *Backend) generateVersionCertificate(version uint) (*versionCertificate, error) {
	vc := &versionCertificate{
		Address:   sb.Address(),
		PublicKey: sb.publicKey,
		Version:   version,
	}
	err := vc.Sign(sb.Sign)
	if err != nil {
		return nil, err
	}
	return vc, nil
}

func (sb *Backend) encodeVersionCertificatesMsg(versionCertificates []*versionCertificate) ([]byte, error) {
	payload, err := rlp.EncodeToBytes(versionCertificates)
	if err != nil {
		return nil, err
	}
	msg := &istanbul.Message{
		Code: istanbul.VersionCertificatesMsg,
		Msg:  payload,
	}
	msgPayload, err := msg.Payload()
	if err != nil {
		return nil, err
	}
	return msgPayload, nil
}

func (sb *Backend) gossipVersionCertificatesMsg(versionCertificates []*versionCertificate) error {
	logger := sb.logger.New("func", "gossipVersionCertificatesMsg")

	payload, err := sb.encodeVersionCertificatesMsg(versionCertificates)
	if err != nil {
		logger.Warn("Error encoding version certificate msg", "err", err)
		return err
	}
	return sb.Gossip(payload, istanbul.VersionCertificatesMsg)
}

func (sb *Backend) getAllVersionCertificates() ([]*versionCertificate, error) {
	allEntries, err := sb.versionCertificateTable.GetAll()
	if err != nil {
		return nil, err
	}
	allVersionCertificates := make([]*versionCertificate, len(allEntries))
	for i, entry := range allEntries {
		allVersionCertificates[i] = newVersionCertificateFromEntry(entry)
	}
	return allVersionCertificates, nil
}

// sendVersionCertificateTable sends all VersionCertificates this node
// has to a peer
func (sb *Backend) sendVersionCertificateTable(peer consensus.Peer) error {
	logger := sb.logger.New("func", "sendVersionCertificateTable")
	allVersionCertificates, err := sb.getAllVersionCertificates()
	if err != nil {
		logger.Warn("Error getting all version certificates", "err", err)
		return err
	}
	payload, err := sb.encodeVersionCertificatesMsg(allVersionCertificates)
	if err != nil {
		logger.Warn("Error encoding version certificate msg", "err", err)
		return err
	}
	return peer.Send(istanbul.VersionCertificatesMsg, payload)
}

func (sb *Backend) handleVersionCertificatesMsg(addr common.Address, peer consensus.Peer, payload []byte) error {
	logger := sb.logger.New("func", "handleVersionCertificatesMsg")
	logger.Trace("Handling version certificates msg")

	// Since this is a gossiped messaged, mark that the peer gossiped it (and presumably processed it) and check to see if this node already processed it
	sb.markMessageProcessedByPeer(addr, payload)
	if sb.checkIfMessageProcessedBySelf(payload) {
		return nil
	}
	defer sb.markMessageProcessedBySelf(payload)

	var msg istanbul.Message
	if err := msg.FromPayload(payload, nil); err != nil {
		logger.Error("Error in decoding version certificates message", "err", err, "payload", hex.EncodeToString(payload))
		return err
	}
	logger = logger.New("msg address", msg.Address)

	var versionCertificates []*versionCertificate
	if err := rlp.DecodeBytes(msg.Msg, &versionCertificates); err != nil {
		logger.Warn("Error in decoding received version certificates msg", "err", err)
		return err
	}

	// If the announce's valAddress is not within the validator connection set, then ignore it
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		logger.Trace("Error in retrieving validator conn set", "err", err)
		return err
	}

	var validEntries []*vet.VersionCertificateEntry
	validAddresses := make(map[common.Address]bool)
	// Verify all entries are valid and remove duplicates
	for _, versionCertificate := range versionCertificates {
		// The public key and address are not RLP encoded/decoded and must be
		// explicitly recovered.
		if err := versionCertificate.RecoverPublicKeyAndAddress(); err != nil {
			logger.Warn("Error recovering version certificates public key and address from signature", "err", err)
			continue
		}
		if !validatorConnSet[versionCertificate.Address] {
			logger.Debug("Found version certificate from an address not in the validator conn set", "address", versionCertificate.Address)
			continue
		}
		if _, ok := validAddresses[versionCertificate.Address]; ok {
			logger.Debug("Found duplicate version certificate in message", "address", versionCertificate.Address)
			continue
		}
		validAddresses[versionCertificate.Address] = true
		validEntries = append(validEntries, versionCertificate.Entry())
	}
	if err := sb.upsertAndGossipVersionCertificateEntries(validEntries); err != nil {
		logger.Warn("Error upserting and gossiping entries", "err", err)
		return err
	}
	return nil
}

func (sb *Backend) upsertAndGossipVersionCertificateEntries(entries []*vet.VersionCertificateEntry) error {
	logger := sb.logger.New("func", "upsertAndGossipVersionCertificateEntries")

	shouldProcess, err := sb.shouldSaveAndPublishValEnodeURLs()
	if err != nil {
		logger.Warn("Error in checking if should process queryEnode", err)
	}
	if shouldProcess {
		// Update entries in val enode db
		var valEnodeEntries []*istanbul.AddressEntry
		for _, entry := range entries {
			// Don't add ourselves into the val enode table
			if entry.Address == sb.Address() {
				continue
			}
			// Update the HighestKnownVersion for this address. Upsert will
			// only update this entry if the HighestKnownVersion is greater
			// than the existing one.
			// Also store the PublicKey for future encryption in queryEnode msgs
			valEnodeEntries = append(valEnodeEntries, &istanbul.AddressEntry{
				Address:             entry.Address,
				PublicKey:           entry.PublicKey,
				HighestKnownVersion: entry.Version,
			})
		}
		if err := sb.valEnodeTable.UpsertHighestKnownVersion(valEnodeEntries); err != nil {
			logger.Warn("Error upserting val enode table entries", "err", err)
		}
	}

	newEntries, err := sb.versionCertificateTable.Upsert(entries)
	if err != nil {
		logger.Warn("Error upserting version certificate table entries", "err", err)
	}

	// Only regossip entries that do not originate from an address that we have
	// gossiped a version certificate for within the last 5 minutes, excluding
	// our own address.
	var versionCertificatesToRegossip []*versionCertificate
	sb.lastVersionCertificatesGossipedMu.Lock()
	for _, entry := range newEntries {
		lastGossipTime, ok := sb.lastVersionCertificatesGossiped[entry.Address]
		if ok && time.Since(lastGossipTime) >= versionCertificateGossipCooldownDuration && entry.Address != sb.ValidatorAddress() {
			continue
		}
		versionCertificatesToRegossip = append(versionCertificatesToRegossip, newVersionCertificateFromEntry(entry))
		sb.lastVersionCertificatesGossiped[entry.Address] = time.Now()
	}
	sb.lastVersionCertificatesGossipedMu.Unlock()
	if len(versionCertificatesToRegossip) > 0 {
		return sb.gossipVersionCertificatesMsg(versionCertificatesToRegossip)
	}
	return nil
}

// UpdateAnnounceVersion will asynchronously update the announce version.
func (sb *Backend) UpdateAnnounceVersion() {
	sb.updateAnnounceVersionCh <- struct{}{}
}

// GetAnnounceVersion will retrieve the current announce version.
func (sb *Backend) GetAnnounceVersion() uint {
	sb.announceVersionMu.RLock()
	defer sb.announceVersionMu.RUnlock()
	return sb.announceVersion
}

// setAndShareUpdatedAnnounceVersion generates announce data structures and
// and shares them with relevant nodes.
// It will:
//  1) Generate a new enode certificate
//  2) Multicast the new enode certificate to all peers in the validator conn set (note that if this is a proxied validator, it's multicast
//     message will be wrapped within a forward message to the proxy (which will in turn send the enode certificate to remote validators).
//  3) Generate a new version certificate
//  4) Gossip the new version certificate to all peers
func (sb *Backend) setAndShareUpdatedAnnounceVersion(version uint) error {
	logger := sb.logger.New("func", "setAndShareUpdatedAnnounceVersion")
	// Send new versioned enode msg to all other registered or elected validators
	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		return err
	}

	// Don't send any of the following messages if this node is not in the validator conn set
	if !validatorConnSet[sb.Address()] {
		logger.Trace("Not in the validator conn set, not updating announce version")
		return nil
	}

	enodeCertificateMsgs, err := sb.generateEnodeCertificateMsgs(version)
	if err != nil {
		return err
	}

	if len(enodeCertificateMsgs) > 0 {
		sb.SetEnodeCertificateMsgMap(enodeCertificateMsgs)
	}

	destAddresses := make([]common.Address, 0, len(validatorConnSet))
	for address := range validatorConnSet {
		destAddresses = append(destAddresses, address)
	}
	if sb.IsProxiedValidator() {
		proxySpecificPayloads := make(map[enode.ID][]byte)

		for proxyID, enodeCertMsg := range enodeCertificateMsgs {
			payload, err := enodeCertMsg.Payload()
			if err != nil {
				logger.Error("Error getting payload of enode certificate message", "err", err, "proxyID", proxyID)
				return err
			} else {
				proxySpecificPayloads[proxyID] = payload
			}
		}

		if err := sb.proxyEngine.SendForwardMsg(nil, destAddresses, istanbul.EnodeCertificateMsg, nil, proxySpecificPayloads); err != nil {
			logger.Error("Error in sharing the enode certificate message to the proxies", "error", err)
			return err
		}
	} else {
		enodeCertMsg := enodeCertificateMsgs[sb.SelfNode().ID()]

		if enodeCertMsg != nil {
			payload, err := enodeCertMsg.Payload()
			if err != nil {
				return err
			}

			// Send the new enode certificate to all of the connected validators
			err = sb.Multicast(destAddresses, payload, istanbul.EnodeCertificateMsg, false)
			if err != nil {
				return err
			}
		}
	}

	// Generate and gossip a new version certificate
	newVersionCertificate, err := sb.generateVersionCertificate(version)
	if err != nil {
		return err
	}
	return sb.upsertAndGossipVersionCertificateEntries([]*vet.VersionCertificateEntry{
		newVersionCertificate.Entry(),
	})
}

func getTimestamp() uint {
	// Unix() returns a int64, but we need a uint for the golang rlp encoding implmentation. Warning: This timestamp value will be truncated in 2106.
	return uint(time.Now().Unix())
}

// RetrieveEnodeCertificateMsgMap gets the most recent enode certificate messages.
// May be nil if no message was generated as a result of the core not being
// started, or if a proxy has not received a message from its proxied validator
func (sb *Backend) RetrieveEnodeCertificateMsgMap() map[enode.ID]*istanbul.Message {
	sb.enodeCertificateMsgMapMu.Lock()
	defer sb.enodeCertificateMsgMapMu.Unlock()
	if sb.enodeCertificateMsgMap == nil {
		return nil
	}
	return sb.enodeCertificateMsgMap
}

func (sb *Backend) GenerateEnodeCertificateMsg(enodeURL string) (*istanbul.Message, error) {
	version := sb.GetAnnounceVersion()

	enodeCertificate := &istanbul.EnodeCertificate{
		EnodeURL: enodeURL,
		Version:  version,
	}
	enodeCertificateBytes, err := rlp.EncodeToBytes(enodeCertificate)
	if err != nil {
		return nil, err
	}
	msg := &istanbul.Message{
		Code:    istanbul.EnodeCertificateMsg,
		Address: sb.Address(),
		Msg:     enodeCertificateBytes,
	}
	// Sign the message
	if err := msg.Sign(sb.Sign); err != nil {
		return nil, err
	}

	return msg, nil
}

// generateEnodeCertificateMsg generates an enode certificate message with the enode
// this node is publicly accessible at. If this node is proxied, the proxy's
// public enode is used.
func (sb *Backend) generateEnodeCertificateMsgs(version uint) (map[enode.ID]*istanbul.Message, error) {
	logger := sb.logger.New("func", "generateEnodeCertificateMsg")

	externalEnodes := make(map[enode.ID]*enode.Node)
	if sb.IsProxiedValidator() {
		if proxies, _, err := sb.proxyEngine.GetProxiesAndValAssignments(); err != nil {
			return nil, err
		} else {
			for _, proxy := range proxies {
				if proxy.IsPeered() {
					externalEnodes[proxy.ExternalNode().ID()] = proxy.ExternalNode()
				}
			}
		}
	} else {
		selfEnode := sb.p2pserver.Self()
		externalEnodes[selfEnode.ID()] = selfEnode
	}

	enodeCertificateMsgs := make(map[enode.ID]*istanbul.Message)
	for externalNodeID, externalNode := range externalEnodes {
		enodeCertificate := &istanbul.EnodeCertificate{
			EnodeURL: externalNode.URLv4(),
			Version:  version,
		}
		enodeCertificateBytes, err := rlp.EncodeToBytes(enodeCertificate)
		if err != nil {
			return nil, err
		}
		msg := &istanbul.Message{
			Code:    istanbul.EnodeCertificateMsg,
			Address: sb.Address(),
			Msg:     enodeCertificateBytes,
		}
		// Sign the message
		if err := msg.Sign(sb.Sign); err != nil {
			return nil, err
		}

		enodeCertificateMsgs[externalNodeID] = msg
	}

	logger.Trace("Generated Istanbul Enode Certificate messages", "enodeCertificateMsgs", enodeCertificateMsgs)
	return enodeCertificateMsgs, nil
}

// handleEnodeCertificateMsg handles an enode certificate message for proxied and standalone validators.
func (sb *Backend) handleEnodeCertificateMsg(peer consensus.Peer, payload []byte) error {
	logger := sb.logger.New("func", "handleEnodeCertificateMsg")

	var msg istanbul.Message
	// Decode payload into msg
	err := msg.FromPayload(payload, istanbul.GetSignatureAddress)
	if err != nil {
		logger.Error("Error in decoding received Istanbul Enode Certificate message", "err", err, "payload", hex.EncodeToString(payload))
		return err
	}
	logger = logger.New("msg address", msg.Address)

	var enodeCertificate istanbul.EnodeCertificate
	if err := rlp.DecodeBytes(msg.Msg, &enodeCertificate); err != nil {
		logger.Warn("Error in decoding received Istanbul Enode Certificate message content", "err", err, "IstanbulMsg", msg.String())
		return err
	}
	logger.Trace("Received Istanbul Enode Certificate message", "enodeCertificate", enodeCertificate)

	parsedNode, err := enode.ParseV4(enodeCertificate.EnodeURL)
	if err != nil {
		logger.Warn("Malformed v4 node in received Istanbul Enode Certificate message", "enodeCertificate", enodeCertificate, "err", err)
		return err
	}

	// Ensure this node is a validator in the validator conn set
	shouldSave, err := sb.shouldSaveAndPublishValEnodeURLs()
	if err != nil {
		logger.Error("Error checking if should save received validator enode url", "err", err)
		return err
	}
	if !shouldSave {
		logger.Debug("This node should not save validator enode urls, ignoring enodeCertificate")
		return nil
	}

	validatorConnSet, err := sb.retrieveValidatorConnSet()
	if err != nil {
		logger.Debug("Error in retrieving registered/elected valset", "err", err)
		return err
	}

	if !validatorConnSet[msg.Address] {
		logger.Debug("Received Istanbul Enode Certificate message originating from a node not in the validator conn set")
		return errUnauthorizedAnnounceMessage
	}

	if err := sb.valEnodeTable.UpsertVersionAndEnode([]*istanbul.AddressEntry{{Address: msg.Address, Node: parsedNode, Version: enodeCertificate.Version}}); err != nil {
		logger.Warn("Error in upserting a val enode table entry", "error", err)
		return err
	}

	if sb.IsProxiedValidator() {
		// Send a valEnodesShare message to the proxy
		sb.proxyEngine.SendValEnodesShareMsgToAllProxies()
	}

	return nil
}

func (sb *Backend) sendEnodeCertificateMsg(peer consensus.Peer, msg *istanbul.Message) error {
	logger := sb.logger.New("func", "sendEnodeCertificateMsg")
	payload, err := msg.Payload()
	if err != nil {
		logger.Error("Error getting payload of enode certificate message", "err", err)
		return err
	}
	return peer.Send(istanbul.EnodeCertificateMsg, payload)
}

// This function will set this nodes enode certificate field and then share it with all it's connected
// validators.
func (sb *Backend) SetEnodeCertificateMsgMap(enodeCertMsgMap map[enode.ID]*istanbul.Message) error {
	logger := sb.logger.New("func", "SetEnodeCertificateMsgMap")
	var enodeCertVersion *uint

	// Verify that all of the certificates have the same version
	for _, enodeCertMsg := range enodeCertMsgMap {
		var enodeCert istanbul.EnodeCertificate
		if err := rlp.DecodeBytes(enodeCertMsg.Msg, &enodeCert); err != nil {
			return err
		}

		if enodeCertVersion == nil {
			enodeCertVersion = &enodeCert.Version
		} else {
			if enodeCert.Version != *enodeCertVersion {
				logger.Error("enode certificate messages within enode certificate msg map don't all have the same version")
				return errInvalidEnodeCertMsgMap
			}
		}
	}

	sb.enodeCertificateMsgMapMu.Lock()
	defer sb.enodeCertificateMsgMapMu.Unlock()

	// Already have a more recent or the same enodeCertificate
	if *enodeCertVersion <= sb.enodeCertificateMsgVersion {
		logger.Info("Ignoring enode certificate msg map since it's an older version")
		return nil
	} else {
		sb.enodeCertificateMsgMap = enodeCertMsgMap
		sb.enodeCertificateMsgVersion = *enodeCertVersion
	}

	return nil
}

func (sb *Backend) getEnodeCertificateMsgVersion() uint {
	sb.enodeCertificateMsgMapMu.RLock()
	defer sb.enodeCertificateMsgMapMu.RUnlock()
	return sb.enodeCertificateMsgVersion
}

func (sb *Backend) GetValEnodeTableEntries(valAddresses []common.Address) (map[common.Address]*istanbul.AddressEntry, error) {
	addressEntries, err := sb.valEnodeTable.GetValEnodes(valAddresses)

	if err != nil {
		return nil, err
	}

	returnMap := make(map[common.Address]*istanbul.AddressEntry)

	for address, addressEntry := range addressEntries {
		returnMap[address] = addressEntry
	}

	return returnMap, nil
}

func (sb *Backend) RewriteValEnodeTableEntries(entries []*istanbul.AddressEntry) error {
	addressesToKeep := make(map[common.Address]bool)

	for _, entry := range entries {
		addressesToKeep[entry.GetAddress()] = true
	}

	sb.valEnodeTable.PruneEntries(addressesToKeep)
	sb.valEnodeTable.UpsertVersionAndEnode(entries)

	return nil
}
