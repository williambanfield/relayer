package relayer

import (
	"fmt"
	"reflect"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	clientutils "github.com/cosmos/ibc-go/v2/modules/core/02-client/client/utils"
	clienttypes "github.com/cosmos/ibc-go/v2/modules/core/02-client/types"
	commitmenttypes "github.com/cosmos/ibc-go/v2/modules/core/23-commitment/types"
	ibctmtypes "github.com/cosmos/ibc-go/v2/modules/light-clients/07-tendermint/types"
	tmclient "github.com/cosmos/ibc-go/v2/modules/light-clients/07-tendermint/types"
	"github.com/tendermint/tendermint/light"
	"golang.org/x/sync/errgroup"
)

// CreateClients creates clients for src on dst and dst on src if the client ids are unspecified.
// TODO: de-duplicate code
func (c *Chain) CreateClients(dst *Chain, allowUpdateAfterExpiry, allowUpdateAfterMisbehaviour, override bool) (modified bool, err error) {
	var (
		eg                               = new(errgroup.Group)
		srcUpdateHeader, dstUpdateHeader *tmclient.Header
	)

	srch, dsth, err := QueryLatestHeights(c, dst)
	if err != nil {
		return false, err
	}

	eg.Go(func() error {
		var err error
		srcUpdateHeader, err = c.GetLightSignedHeaderAtHeight(srch)
		return err
	})
	eg.Go(func() error {
		var err error
		dstUpdateHeader, err = dst.GetLightSignedHeaderAtHeight(dsth)
		return err
	})
	if err = eg.Wait(); err != nil {
		return
	}

	// Create client for the destination chain on the source chain if client id is unspecified
	if c.PathEnd.ClientID == "" {
		if c.debug {
			c.logCreateClient(dst, dstUpdateHeader.Header.Height)
		}
		ubdPeriod, err := dst.QueryUnbondingPeriod()
		if err != nil {
			return modified, err
		}

		// Create the ClientState we want on 'c' tracking 'dst'
		clientState := ibctmtypes.NewClientState(
			dstUpdateHeader.GetHeader().GetChainID(),
			ibctmtypes.NewFractionFromTm(light.DefaultTrustLevel),
			dst.GetTrustingPeriod(),
			ubdPeriod,
			time.Minute*10,
			dstUpdateHeader.GetHeight().(clienttypes.Height),
			commitmenttypes.GetSDKSpecs(),
			DefaultUpgradePath,
			allowUpdateAfterExpiry,
			allowUpdateAfterMisbehaviour,
		)

		var (
			clientID string
			found    bool
		)
		// Will not reuse same client if override is true
		if !override {
			// Check if an identical light client already exists
			clientID, found = FindMatchingClient(c, dst, clientState)
		}
		if !found || override {
			createMsg, err := c.CreateClient(clientState, dstUpdateHeader)
			if err != nil {
				return modified, err
			}

			msgs := []sdk.Msg{createMsg}

			// if a matching client does not exist, create one
			res, success, err := c.SendMsgs(msgs)
			if err != nil {
				c.LogFailedTx(res, err, msgs)
				return modified, err
			}
			if !success {
				c.LogFailedTx(res, err, msgs)
				return modified, fmt.Errorf("tx failed: %s", res.RawLog)
			}

			// update the client identifier
			// use index 0, the transaction only has one message
			clientID, err = ParseClientIDFromEvents(res.Logs[0].Events)
			if err != nil {
				return modified, err
			}
		} else if c.debug {
			c.logIdentifierExists(dst, "client", clientID)
		}

		c.PathEnd.ClientID = clientID
		modified = true

	} else {
		// Ensure client exists in the event of user inputted identifiers
		// TODO: check client is not expired
		_, err := c.QueryClientStateResponse(srcUpdateHeader.Header.Height)
		if err != nil {
			return false, fmt.Errorf("please ensure provided on-chain client (%s) exists on the chain (%s): %v",
				c.PathEnd.ClientID, c.ChainID, err)
		}
	}

	// Create client for the source chain on destination chain if client id is unspecified
	if dst.PathEnd.ClientID == "" {
		if dst.debug {
			dst.logCreateClient(c, srcUpdateHeader.Header.Height)
		}
		ubdPeriod, err := c.QueryUnbondingPeriod()
		if err != nil {
			return modified, err
		}
		// Create the ClientState we want on 'dst' tracking 'c'
		clientState := ibctmtypes.NewClientState(
			srcUpdateHeader.GetHeader().GetChainID(),
			ibctmtypes.NewFractionFromTm(light.DefaultTrustLevel),
			c.GetTrustingPeriod(),
			ubdPeriod,
			time.Minute*10,
			srcUpdateHeader.GetHeight().(clienttypes.Height),
			commitmenttypes.GetSDKSpecs(),
			DefaultUpgradePath,
			allowUpdateAfterExpiry,
			allowUpdateAfterMisbehaviour,
		)

		var (
			clientID string
			found    bool
		)
		// Will not reuse same client if override is true
		if !override {
			// Check if an identical light client already exists
			// NOTE: we pass in 'dst' as the source and 'c' as the
			// counterparty.
			clientID, found = FindMatchingClient(dst, c, clientState)
		}
		if !found || override {
			createMsg, err := dst.CreateClient(clientState, srcUpdateHeader)
			if err != nil {
				return modified, err
			}

			msgs := []sdk.Msg{createMsg}

			// if a matching client does not exist, create one
			res, success, err := dst.SendMsgs(msgs)
			if err != nil {
				dst.LogFailedTx(res, err, msgs)
				return modified, err
			}
			if !success {
				dst.LogFailedTx(res, err, msgs)
				return modified, fmt.Errorf("tx failed: %s", res.RawLog)
			}

			// update client identifier
			clientID, err = ParseClientIDFromEvents(res.Logs[0].Events)
			if err != nil {
				return modified, err
			}
		} else if c.debug {
			c.logIdentifierExists(dst, "client", clientID)
		}

		dst.PathEnd.ClientID = clientID
		modified = true

	} else {
		// Ensure client exists in the event of user inputted identifiers
		// TODO: check client is not expired
		_, err := dst.QueryClientStateResponse(dstUpdateHeader.Header.Height)
		if err != nil {
			return false, fmt.Errorf("please ensure provided on-chain client (%s) exists on the chain (%s): %v",
				dst.PathEnd.ClientID, dst.ChainID, err)
		}

	}

	c.Log(fmt.Sprintf("★ Clients created: client(%s) on chain[%s] and client(%s) on chain[%s]",
		c.PathEnd.ClientID, c.ChainID, dst.PathEnd.ClientID, dst.ChainID))

	return modified, nil
}

// UpdateClients updates clients for src on dst and dst on src given the configured paths
func (c *Chain) UpdateClients(dst *Chain) (err error) {
	var (
		clients                          = &RelayMsgs{Src: []sdk.Msg{}, Dst: []sdk.Msg{}}
		eg                               = new(errgroup.Group)
		srcUpdateHeader, dstUpdateHeader *tmclient.Header
	)

	srch, dsth, err := QueryLatestHeights(c, dst)
	if err != nil {
		return err
	}

	eg.Go(func() error {
		srcUpdateHeader, err = c.GetLightSignedHeaderAtHeight(srch)
		return err
	})
	eg.Go(func() error {
		dstUpdateHeader, err = dst.GetLightSignedHeaderAtHeight(dsth)
		return err
	})
	if err = eg.Wait(); err != nil {
		return
	}

	srcUpdateMsg, err := c.UpdateClient(dst, dstUpdateHeader)
	if err != nil {
		return err
	}
	dstUpdateMsg, err := dst.UpdateClient(c, srcUpdateHeader)
	if err != nil {
		return err
	}

	clients.Src = append(clients.Src, srcUpdateMsg)
	clients.Dst = append(clients.Dst, dstUpdateMsg)

	// Send msgs to both chains
	if clients.Ready() {
		if clients.Send(c, dst); clients.Success() {
			c.Log(fmt.Sprintf("★ Clients updated: [%s]client(%s) {%d}->{%d} and [%s]client(%s) {%d}->{%d}",
				c.ChainID,
				c.PathEnd.ClientID,
				MustGetHeight(srcUpdateHeader.TrustedHeight),
				srcUpdateHeader.Header.Height,
				dst.ChainID,
				dst.PathEnd.ClientID,
				MustGetHeight(dstUpdateHeader.TrustedHeight),
				dstUpdateHeader.Header.Height,
			),
			)
		}
	}

	return nil
}

// UpgradeClients upgrades the client on src after dst chain has undergone an upgrade.
func (c *Chain) UpgradeClients(dst *Chain, height int64) error {
	dstHeader, err := dst.GetLightSignedHeaderAtHeight(height)
	if err != nil {
		return err
	}

	// updates off-chain light client
	updateMsg, err := c.UpdateClient(dst, dstHeader)
	if err != nil {
		return err
	}

	if height == 0 {
		height, err = dst.QueryLatestHeight()
		if err != nil {
			return err
		}
	}

	// query proofs on counterparty
	clientState, proofUpgradeClient, _, err := dst.QueryUpgradedClient(height)
	if err != nil {
		return err
	}

	consensusState, proofUpgradeConsensusState, _, err := dst.QueryUpgradedConsState(height)
	if err != nil {
		return err
	}

	upgradeMsg := &clienttypes.MsgUpgradeClient{ClientId: c.PathEnd.ClientID, ClientState: clientState,
		ConsensusState: consensusState, ProofUpgradeClient: proofUpgradeClient,
		ProofUpgradeConsensusState: proofUpgradeConsensusState, Signer: c.MustGetAddress()}

	msgs := []sdk.Msg{
		updateMsg,
		upgradeMsg,
	}

	res, _, err := c.SendMsgs(msgs)
	if err != nil {
		c.LogFailedTx(res, err, msgs)
		return err
	}

	return nil
}

// FindMatchingClient will determine if there exists a client with identical client and consensus states
// to the client which would have been created. Source is the chain that would be adding a client
// which would track the counterparty. Therefore we query source for the existing clients
// and check if any match the counterparty. The counterparty must have a matching consensus state
// to the latest consensus state of a potential match. The provided client state is the client
// state that will be created if there exist no matches.
func FindMatchingClient(source, counterparty *Chain, clientState *ibctmtypes.ClientState) (string, bool) {
	// TODO: add appropriate offset and limits, along with retries
	clientsResp, err := source.QueryClients(DefaultPageRequest())
	if err != nil {
		if source.debug {
			source.Log(fmt.Sprintf("Error: querying clients on %s failed: %v", source.PathEnd.ChainID, err))
		}
		return "", false
	}

	for _, identifiedClientState := range clientsResp.ClientStates {
		// unpack any into ibc tendermint client state
		existingClientState, err := CastClientStateToTMType(identifiedClientState.ClientState)
		if err != nil {
			return "", false
		}

		// check if the client states match
		// NOTE: FrozenHeight.IsZero() is a sanity check, the client to be created should always
		// have a zero frozen height and therefore should never match with a frozen client
		if IsMatchingClient(*clientState, *existingClientState) && existingClientState.FrozenHeight.IsZero() {

			// query the latest consensus state of the potential matching client
			consensusStateResp, err := clientutils.QueryConsensusStateABCI(source.CLIContext(0),
				identifiedClientState.ClientId, existingClientState.GetLatestHeight())
			if err != nil {
				if source.debug {
					source.Log(fmt.Sprintf("Error: failed to query latest consensus state for existing client on chain %s: %v",
						source.PathEnd.ChainID, err))
				}
				continue
			}

			//nolint:lll
			header, err := counterparty.GetLightSignedHeaderAtHeight(int64(existingClientState.GetLatestHeight().GetRevisionHeight()))
			if err != nil {
				if source.debug {
					source.Log(fmt.Sprintf("Error: failed to query header for chain %s at height %d: %v",
						counterparty.PathEnd.ChainID, existingClientState.GetLatestHeight().GetRevisionHeight(), err))
				}
				continue
			}

			exportedConsState, err := clienttypes.UnpackConsensusState(consensusStateResp.ConsensusState)
			if err != nil {
				if source.debug {
					source.Log(fmt.Sprintf("Error: failed to consensus state on chain %s: %v", counterparty.PathEnd.ChainID, err))
				}
				continue
			}
			existingConsensusState, ok := exportedConsState.(*ibctmtypes.ConsensusState)
			if !ok {
				if source.debug {
					source.Log(fmt.Sprintf("Error:consensus state is not tendermint type on chain %s", counterparty.PathEnd.ChainID))
				}
				continue
			}

			if existingClientState.IsExpired(existingConsensusState.Timestamp, time.Now()) {
				continue
			}

			if IsMatchingConsensusState(existingConsensusState, header.ConsensusState()) {
				// found matching client
				return identifiedClientState.ClientId, true
			}
		}
	}

	return "", false
}

// IsMatchingClient determines if the two provided clients match in all fields
// except latest height. They are assumed to be IBC tendermint light clients.
// NOTE: we don't pass in a pointer so upstream references don't have a modified
// latest height set to zero.
func IsMatchingClient(clientStateA, clientStateB ibctmtypes.ClientState) bool {
	// zero out latest client height since this is determined and incremented
	// by on-chain updates. Changing the latest height does not fundamentally
	// change the client. The associated consensus state at the latest height
	// determines this last check
	clientStateA.LatestHeight = clienttypes.ZeroHeight()
	clientStateB.LatestHeight = clienttypes.ZeroHeight()

	return reflect.DeepEqual(clientStateA, clientStateB)
}

// IsMatchingConsensusState determines if the two provided consensus states are
// identical. They are assumed to be IBC tendermint light clients.
func IsMatchingConsensusState(consensusStateA, consensusStateB *ibctmtypes.ConsensusState) bool {
	return reflect.DeepEqual(*consensusStateA, *consensusStateB)
}

// AutoUpdateClient update client automatically to prevent expiry
func AutoUpdateClient(src, dst *Chain, thresholdTime time.Duration) (time.Duration, error) {
	srch, dsth, err := QueryLatestHeights(src, dst)
	if err != nil {
		return 0, err
	}

	clientState, err := src.QueryTMClientState(srch)
	if err != nil {
		return 0, err
	}

	if clientState.TrustingPeriod <= thresholdTime {
		return 0, fmt.Errorf("client (%s) trusting period time is less than or equal to threshold time",
			src.PathEnd.ClientID)
	}

	// query the latest consensus state of the potential matching client
	consensusStateResp, err := clientutils.QueryConsensusStateABCI(src.CLIContext(0),
		src.PathEnd.ClientID, clientState.GetLatestHeight())
	if err != nil {
		return 0, err
	}

	exportedConsState, err := clienttypes.UnpackConsensusState(consensusStateResp.ConsensusState)
	if err != nil {
		return 0, err
	}

	consensusState, ok := exportedConsState.(*ibctmtypes.ConsensusState)
	if !ok {
		return 0, fmt.Errorf("consensus state with clientID %s from chain %s is not IBC tendermint type",
			src.PathEnd.ClientID, src.PathEnd.ChainID)
	}

	expirationTime := consensusState.Timestamp.Add(clientState.TrustingPeriod)

	timeToExpiry := time.Until(expirationTime)

	if timeToExpiry > thresholdTime {
		return timeToExpiry, nil
	}

	if clientState.IsExpired(consensusState.Timestamp, time.Now()) {
		return 0, fmt.Errorf("client (%s) is already expired on chain: %s", src.PathEnd.ClientID, src.ChainID)
	}

	srcUpdateHeader, err := src.GetIBCUpdateHeader(dst, srch)
	if err != nil {
		return 0, err
	}

	dstUpdateHeader, err := dst.GetIBCUpdateHeader(src, dsth)
	if err != nil {
		return 0, err
	}

	updateMsg, err := src.UpdateClient(dst, dstUpdateHeader)
	if err != nil {
		return 0, err
	}

	msgs := []sdk.Msg{updateMsg}

	res, success, err := src.SendMsgs(msgs)
	if err != nil {
		src.LogFailedTx(res, err, msgs)
		return 0, err
	}
	if !success {
		return 0, fmt.Errorf("tx failed: %s", res.RawLog)
	}
	src.Log(fmt.Sprintf("★ Client updated: [%s]client(%s) {%d}->{%d}",
		src.ChainID,
		src.PathEnd.ClientID,
		MustGetHeight(srcUpdateHeader.TrustedHeight),
		srcUpdateHeader.Header.Height,
	))

	return clientState.TrustingPeriod, nil
}
