package relay

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cosmos/cosmos-sdk/codec"
	clienttypes "github.com/cosmos/ibc-go/v7/modules/core/02-client/types"
	"github.com/cosmos/ibc-go/v7/modules/core/exported"
	lcptypes "github.com/datachainlab/lcp-go/light-clients/lcp/types"
	"github.com/datachainlab/lcp-go/relay/elc"
	"github.com/datachainlab/lcp-go/relay/enclave"
	"github.com/datachainlab/lcp-go/sgx/ias"
	"github.com/ethereum/go-ethereum/common"
	"github.com/hyperledger-labs/yui-relayer/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Prover struct {
	config       ProverConfig
	originChain  core.Chain
	originProver core.Prover

	homePath string
	codec    codec.ProtoCodecMarshaler
	path     *core.PathEnd

	lcpServiceClient LCPServiceClient

	// state
	// registered key info for requesting lcp to generate proof.
	activeEnclaveKey *enclave.EnclaveKeyInfo
	// if not nil, the key is finalized.
	// if nil, the key is not finalized yet.
	unfinalizedMsgID core.MsgID
}

var (
	_ core.Prover = (*Prover)(nil)
)

func NewProver(config ProverConfig, originChain core.Chain, originProver core.Prover) (*Prover, error) {
	conn, err := grpc.Dial(
		config.LcpServiceAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, err
	}
	return &Prover{config: config, originChain: originChain, originProver: originProver, lcpServiceClient: NewLCPServiceClient(conn)}, nil
}

func (pr *Prover) GetOriginProver() core.Prover {
	return pr.originProver
}

// Init initializes the chain
func (pr *Prover) Init(homePath string, timeout time.Duration, codec codec.ProtoCodecMarshaler, debug bool) error {
	if debug {
		ias.SetAllowDebugEnclaves()
	}
	if err := pr.originChain.Init(homePath, timeout, codec, debug); err != nil {
		return err
	}
	if err := pr.originProver.Init(homePath, timeout, codec, debug); err != nil {
		return err
	}
	if err := os.MkdirAll(pr.dbPath(), os.ModePerm); err != nil {
		return err
	}
	pr.homePath = homePath
	pr.codec = codec
	return nil
}

// SetRelayInfo sets source's path and counterparty's info to the chain
func (pr *Prover) SetRelayInfo(path *core.PathEnd, counterparty *core.ProvableChain, counterpartyPath *core.PathEnd) error {
	pr.path = path
	return nil
}

// SetupForRelay performs chain-specific setup before starting the relay
func (pr *Prover) SetupForRelay(ctx context.Context) error {
	return nil
}

// GetChainID returns the chain ID
func (pr *Prover) GetChainID() string {
	return pr.originChain.ChainID()
}

// CreateInitialLightClientState returns a pair of ClientState and ConsensusState based on the state of the self chain at `height`.
// These states will be submitted to the counterparty chain as MsgCreateClient.
// If `height` is nil, the latest finalized height is selected automatically.
func (pr *Prover) CreateInitialLightClientState(height exported.Height) (exported.ClientState, exported.ConsensusState, error) {
	// NOTE: Query the LCP for available keys, but no need to register it into on-chain here
	tmpEKI, err := pr.selectNewEnclaveKey(context.TODO())
	if err != nil {
		return nil, nil, err
	}
	originClientState, originConsensusState, err := pr.originProver.CreateInitialLightClientState(height)
	if err != nil {
		return nil, nil, err
	}
	anyOriginClientState, err := clienttypes.PackClientState(originClientState)
	if err != nil {
		return nil, nil, err
	}
	anyOriginConsensusState, err := clienttypes.PackConsensusState(originConsensusState)
	if err != nil {
		return nil, nil, err
	}
	res, err := pr.lcpServiceClient.CreateClient(context.TODO(), &elc.MsgCreateClient{
		ClientState:    anyOriginClientState,
		ConsensusState: anyOriginConsensusState,
		Signer:         tmpEKI.EnclaveKeyAddress,
	})
	if err != nil {
		return nil, nil, err
	}

	// TODO relayer should persist res.ClientId
	if pr.config.ElcClientId != res.ClientId {
		return nil, nil, fmt.Errorf("you must specify '%v' as elc_client_id, but got %v", res.ClientId, pr.config.ElcClientId)
	}

	clientState := &lcptypes.ClientState{
		LatestHeight:         clienttypes.Height{},
		Mrenclave:            pr.config.GetMrenclave(),
		KeyExpiration:        pr.config.KeyExpiration,
		AllowedQuoteStatuses: pr.config.AllowedQuoteStatuses,
		AllowedAdvisoryIds:   pr.config.AllowedAdvisoryIds,
	}
	consensusState := &lcptypes.ConsensusState{}
	// NOTE after creates client, register an enclave key into the client state
	return clientState, consensusState, nil
}

// GetLatestFinalizedHeader returns the latest finalized header on this chain
// The returned header is expected to be the latest one of headers that can be verified by the light client
func (pr *Prover) GetLatestFinalizedHeader() (core.Header, error) {
	return pr.originProver.GetLatestFinalizedHeader()
}

// SetupHeadersForUpdate returns the finalized header and any intermediate headers needed to apply it to the client on the counterpaty chain
// The order of the returned header slice should be as: [<intermediate headers>..., <update header>]
// if the header slice's length == nil and err == nil, the relayer should skips the update-client
func (pr *Prover) SetupHeadersForUpdate(dstChain core.FinalityAwareChain, latestFinalizedHeader core.Header) ([]core.Header, error) {
	if err := pr.UpdateEKIfNeeded(context.TODO(), dstChain); err != nil {
		return nil, err
	}

	headers, err := pr.originProver.SetupHeadersForUpdate(dstChain, latestFinalizedHeader)
	if err != nil {
		return nil, err
	}
	if len(headers) == 0 {
		return nil, nil
	}
	var updates []core.Header
	for _, h := range headers {
		anyHeader, err := clienttypes.PackClientMessage(h)
		if err != nil {
			return nil, err
		}
		res, err := pr.lcpServiceClient.UpdateClient(context.TODO(), &elc.MsgUpdateClient{
			ClientId:     pr.config.ElcClientId,
			Header:       anyHeader,
			IncludeState: false,
			Signer:       pr.activeEnclaveKey.EnclaveKeyAddress,
		})
		if err != nil {
			return nil, err
		}
		// ensure the message is valid
		if _, err := lcptypes.EthABIDecodeHeaderedMessage(res.Message); err != nil {
			return nil, err
		}
		updates = append(updates, &lcptypes.UpdateClientMessage{
			ElcMessage: res.Message,
			Signer:     res.Signer,
			Signature:  res.Signature,
		})
	}
	return updates, nil
}

func (pr *Prover) CheckRefreshRequired(counterparty core.ChainInfoICS02Querier) (bool, error) {
	return pr.originProver.CheckRefreshRequired(counterparty)
}

func (pr *Prover) ProveState(ctx core.QueryContext, path string, value []byte) ([]byte, clienttypes.Height, error) {
	proof, proofHeight, err := pr.originProver.ProveState(ctx, path, value)
	if err != nil {
		return nil, clienttypes.Height{}, err
	}
	res, err := pr.lcpServiceClient.VerifyMembership(ctx.Context(), &elc.MsgVerifyMembership{
		ClientId:    pr.config.ElcClientId,
		Prefix:      []byte(exported.StoreKey),
		Path:        path,
		Value:       value,
		ProofHeight: proofHeight,
		Proof:       proof,
		Signer:      pr.activeEnclaveKey.EnclaveKeyAddress,
	})
	if err != nil {
		return nil, clienttypes.Height{}, err
	}
	message, err := lcptypes.EthABIDecodeHeaderedMessage(res.Message)
	if err != nil {
		return nil, clienttypes.Height{}, err
	}
	sc, err := message.GetVerifyMembershipMessage()
	if err != nil {
		return nil, clienttypes.Height{}, err
	}
	cp, err := lcptypes.EthABIEncodeCommitmentProof(&lcptypes.CommitmentProof{
		Message:   res.Message,
		Signer:    common.BytesToAddress(res.Signer),
		Signature: res.Signature,
	})
	if err != nil {
		return nil, clienttypes.Height{}, err
	}
	return cp, sc.Height, nil
}
