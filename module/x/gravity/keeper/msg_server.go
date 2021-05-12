package keeper

import (
	"context"
	"encoding/hex"
	"fmt"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/cosmos/gravity-bridge/module/x/gravity/types"
)

type msgServer struct {
	Keeper
}

// NewMsgServerImpl returns an implementation of the gov MsgServer interface
// for the provided Keeper.
func NewMsgServerImpl(keeper Keeper) types.MsgServer {
	return &msgServer{Keeper: keeper}
}

var _ types.MsgServer = msgServer{}

func (k msgServer) SetDelegateKeys(c context.Context, msg *types.MsgDelegateKeys) (*types.MsgDelegateKeysResponse, error) {
	// ensure that this passes validation
	err := msg.ValidateBasic()
	if err != nil {
		return nil, err
	}

	ctx := sdk.UnwrapSDKContext(c)
	val, _ := sdk.ValAddressFromBech32(msg.ValidatorAddress)
	orch, _ := sdk.AccAddressFromBech32(msg.OrchestratorAddress)

	// ensure that the validator exists
	if k.Keeper.StakingKeeper.Validator(ctx, val) == nil {
		return nil, sdkerrors.Wrap(stakingtypes.ErrNoValidatorFound, val.String())
	}

	// TODO consider impact of maliciously setting duplicate delegate
	// addresses since no signatures from the private keys of these addresses
	// are required for this message it could be sent in a hostile way.

	// set the orchestrator address
	k.SetOrchestratorValidator(ctx, val, orch)
	// set the ethereum address
	k.SetEthAddress(ctx, val, msg.EthereumAddress)

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, msg.Type()),
			sdk.NewAttribute(types.AttributeKeySetOperatorAddr, orch.String()),
		),
	)

	return &types.MsgDelegateKeysResponse{}, nil

}

// SubmitEthereumSignature handles MsgSubmitEthereumSignature
// TODO: check MsgSubmitEthereumSignature to have an Orchestrator field instead of a Validator field
func (k msgServer) SubmitEthereumSignature(c context.Context, msg *types.MsgSubmitEthereumSignature) (*types.MsgSubmitEthereumSignatureResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)

	signature, err := types.UnpackSignature(msg.Signature)
	if err != nil {
		return nil, err
	}
	nonce := sdk.BigEndianToUint64(signature.GetStoreIndex())

	valset := k.GetSignerSetTx(ctx, nonce)
	if valset == nil {
		return nil, sdkerrors.Wrap(types.ErrInvalid, "couldn't find valset")
	}

	gravityID := k.GetGravityID(ctx)
	checkpoint, err := valset.GetCheckpoint([]byte(gravityID))
	if err != nil {
		return nil, err
	}

	sigBytes, err := hex.DecodeString(msg.Signer)
	if err != nil {
		return nil, sdkerrors.Wrap(types.ErrInvalid, "signature decoding")
	}

	orchaddr, _ := sdk.AccAddressFromBech32(msg.Signer)
	validator := k.GetOrchestratorValidator(ctx, orchaddr)
	if validator == nil {
		return nil, sdkerrors.Wrap(types.ErrUnknown, "validator")
	}

	ethAddress := k.GetEthAddress(ctx, validator)
	if ethAddress == "" {
		return nil, sdkerrors.Wrap(types.ErrEmpty, "eth address")
	}

	if err = types.ValidateEthereumSignature(checkpoint, sigBytes, ethAddress); err != nil {
		return nil, sdkerrors.Wrap(types.ErrInvalid, fmt.Sprintf("signature verification failed expected sig by %s with gravity-id %s with checkpoint %s found %s", ethAddress, gravityID, hex.EncodeToString(checkpoint), msg.Signature))
	}

	// persist signature
	if k.GetEthereumSignature(ctx, signature.GetStoreIndex(), validator) != nil {
		return nil, sdkerrors.Wrap(types.ErrDuplicate, "signature duplicate")
	}
	key := k.SetEthereumSignature(ctx, signature, validator)

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, msg.Type()),
			sdk.NewAttribute(types.AttributeKeyValsetConfirmKey, string(key)),
		),
	)

	return &types.MsgSubmitEthereumSignatureResponse{}, nil
}

func (k msgServer) getMsgValidator(ctx sdk.Context, signerString string) (sdk.ValAddress, error) {
	signer, _ := sdk.AccAddressFromBech32(signerString)

	var validatorI stakingtypes.ValidatorI
	validator := k.GetOrchestratorValidator(ctx, signer)
	if validator == nil {
		validatorI = k.StakingKeeper.Validator(ctx, sdk.ValAddress(signer))
	} else {
		validatorI = k.StakingKeeper.Validator(ctx, validator)
	}

	if validatorI == nil {
		return nil, sdkerrors.Wrap(types.ErrUnknown, "not orchestrator or validator")
	} else if !validatorI.IsBonded() {
		return nil, sdkerrors.Wrap(types.ErrUnbonded, fmt.Sprintf("validator: %s", validatorI.GetOperator()))
	}

	return validatorI.GetOperator(), nil
}

func (k msgServer) SubmitEthereumEvent(c context.Context, msg *types.MsgSubmitEthereumEvent) (*types.MsgSubmitEthereumEventResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)

	event, err := types.UnpackEvent(msg.Event)
	if err != nil {
		return nil, err
	}

	// return an error if the validator isn't in the active set
	val, err  := k.getMsgValidator(ctx, msg.Signer)
	if err != nil {
		return nil, err
	}



	// Add the claim to the store
	_, err = k.RecordEventVote(ctx, event, val)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "create attestation")
	}


	// Emit the handle message event
	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, fmt.Sprintf("%T", event)),
			// TODO: maybe return something better here? is this the right string representation?
			sdk.NewAttribute(types.AttributeKeyAttestationID, string(types.GetEthereumEventVoteRecordKey(event.GetNonce(), event.Hash()))),
		),
	)

	return &types.MsgSubmitEthereumEventResponse{}, nil
}

// SendToEthereum handles MsgSendToEthereum
func (k msgServer) SendToEthereum(c context.Context, msg *types.MsgSendToEthereum) (*types.MsgSendToEthereumResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)
	sender, err := sdk.AccAddressFromBech32(msg.Sender)
	if err != nil {
		return nil, err
	}
	txID, err := k.AddToOutgoingPool(ctx, sender, msg.EthereumRecipient, msg.Amount, msg.BridgeFee)
	if err != nil {
		return nil, err
	}

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, msg.Type()),
			sdk.NewAttribute(types.AttributeKeyOutgoingTXID, fmt.Sprint(txID)),
		),
	)

	return &types.MsgSendToEthereumResponse{}, nil
}

// RequestBatchTx handles MsgRequestBatchTx
func (k msgServer) RequestBatchTx(c context.Context, msg *types.MsgRequestBatchTx) (*types.MsgRequestBatchTxResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)

	// Check if the denom is a gravity coin, if not, check if there is a deployed ERC20 representing it.
	// If not, error out
	_, tokenContract, err := k.DenomToERC20Lookup(ctx, msg.Denom)
	if err != nil {
		return nil, err
	}

	batchID, err := k.BuildBatchTx(ctx, tokenContract, BatchTxSize)
	if err != nil {
		return nil, err
	}

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, msg.Type()),
			sdk.NewAttribute(types.AttributeKeyBatchNonce, fmt.Sprint(batchID.Nonce)),
		),
	)

	return &types.MsgRequestBatchTxResponse{}, nil
}

//
//// ConfirmBatch handles MsgConfirmBatch
//func (k msgServer) ConfirmBatch(c context.Context, msg *types.MsgConfirmBatch) (*types.MsgConfirmBatchResponse, error) {
//	ctx := sdk.UnwrapSDKContext(c)
//
//	// fetch the outgoing batch given the nonce
//	batch := k.GetBatchTx(ctx, msg.TokenContract, msg.Nonce)
//	if batch == nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, "couldn't find batch")
//	}
//
//	gravityID := k.GetGravityID(ctx)
//	checkpoint, err := batch.GetCheckpoint(gravityID)
//	if err != nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, "checkpoint generation")
//	}
//
//	sigBytes, err := hex.DecodeString(msg.Signature)
//	if err != nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, "signature decoding")
//	}
//
//	orchaddr, _ := sdk.AccAddressFromBech32(msg.Orchestrator)
//	validator := k.GetOrchestratorValidator(ctx, orchaddr)
//	if validator == nil {
//		return nil, sdkerrors.Wrap(types.ErrUnknown, "validator")
//	}
//
//	ethAddress := k.GetEthAddress(ctx, validator)
//	if ethAddress == "" {
//		return nil, sdkerrors.Wrap(types.ErrEmpty, "eth address")
//	}
//
//	err = types.ValidateEthereumSignature(checkpoint, sigBytes, ethAddress)
//	if err != nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, fmt.Sprintf("signature verification failed expected sig by %s with gravity-id %s with checkpoint %s found %s", ethAddress, gravityID, hex.EncodeToString(checkpoint), msg.Signature))
//	}
//
//	// check if we already have this confirm
//	if k.GetBatchConfirm(ctx, msg.Nonce, msg.TokenContract, orchaddr) != nil {
//		return nil, sdkerrors.Wrap(types.ErrDuplicate, "duplicate signature")
//	}
//	key := k.SetBatchConfirm(ctx, msg)
//
//	ctx.EventManager().EmitEvent(
//		sdk.NewEvent(
//			sdk.EventTypeMessage,
//			sdk.NewAttribute(sdk.AttributeKeyModule, msg.Type()),
//			sdk.NewAttribute(types.AttributeKeyBatchConfirmKey, string(key)),
//		),
//	)
//
//	return nil, nil
//}
//
//// ConfirmLogicCall handles MsgConfirmLogicCall
//func (k msgServer) ConfirmLogicCall(c context.Context, msg *types.MsgConfirmLogicCall) (*types.MsgConfirmLogicCallResponse, error) {
//	ctx := sdk.UnwrapSDKContext(c)
//	invalidationIdBytes, err := hex.DecodeString(msg.InvalidationScope)
//	if err != nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, "invalidation id encoding")
//	}
//
//	// fetch the outgoing logic given the nonce
//	logic := k.GetContractCallTx(ctx, invalidationIdBytes, msg.InvalidationNonce)
//	if logic == nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, "couldn't find logic")
//	}
//
//	gravityID := k.GetGravityID(ctx)
//	checkpoint, err := logic.GetCheckpoint(gravityID)
//	if err != nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, "checkpoint generation")
//	}
//
//	sigBytes, err := hex.DecodeString(msg.Signature)
//	if err != nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, "signature decoding")
//	}
//
//	orchaddr, _ := sdk.AccAddressFromBech32(msg.Orchestrator)
//	validator := k.GetOrchestratorValidator(ctx, orchaddr)
//	if validator == nil {
//		return nil, sdkerrors.Wrap(types.ErrUnknown, "validator")
//	}
//
//	ethAddress := k.GetEthAddress(ctx, validator)
//	if ethAddress == "" {
//		return nil, sdkerrors.Wrap(types.ErrEmpty, "eth address")
//	}
//
//	err = types.ValidateEthereumSignature(checkpoint, sigBytes, ethAddress)
//	if err != nil {
//		return nil, sdkerrors.Wrap(types.ErrInvalid, fmt.Sprintf("signature verification failed expected sig by %s with gravity-id %s with checkpoint %s found %s", ethAddress, gravityID, hex.EncodeToString(checkpoint), msg.Signature))
//	}
//
//	// check if we already have this confirm
//	if k.GetContractCallTxSignature(ctx, invalidationIdBytes, msg.InvalidationNonce, orchaddr) != nil {
//		return nil, sdkerrors.Wrap(types.ErrDuplicate, "duplicate signature")
//	}
//
//	k.SetContractCallTxSignature(ctx, msg)
//
//	ctx.EventManager().EmitEvent(
//		sdk.NewEvent(
//			sdk.EventTypeMessage,
//			sdk.NewAttribute(sdk.AttributeKeyModule, msg.Type()),
//		),
//	)
//
//	return nil, nil
//}

// sendToCosmosEvent handles MsgDepositClaim
// TODO it is possible to submit an old msgDepositClaim (old defined as covering an event nonce that has already been
// executed aka 'observed' and had it's slashing window expire) that will never be cleaned up in the endblocker. This
// should not be a security risk as 'old' events can never execute but it does store spam in the chain.
func (k msgServer) sendToCosmosEvent(c context.Context, signer sdk.ValAddress, event *types.SendToCosmosEvent) (*types.MsgSubmitEthereumEventResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)

	any, err := codectypes.NewAnyWithValue(event)
	if err != nil {
		return nil, err
	}

	// Add the claim to the store
	_, err = k.RecordEventVote(ctx, event, any)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "create attestation")
	}

	// Emit the handle message event
	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, fmt.Sprintf("%T", event)),
			// TODO: maybe return something better here? is this the right string representation?
			sdk.NewAttribute(types.AttributeKeyAttestationID, string(types.GetEthereumEventVoteRecordKey(event.EventNonce, event.Hash()))),
		),
	)

	return &types.MsgSubmitEthereumEventResponse{}, nil
}

// batchExecuted handles MsgWithdrawClaim
// TODO it is possible to submit an old msgWithdrawClaim (old defined as covering an event nonce that has already been
// executed aka 'observed' and had it's slashing window expire) that will never be cleaned up in the endblocker. This
// should not be a security risk as 'old' events can never execute but it does store spam in the chain.
func (k msgServer) batchExecuted(c context.Context, validator sdk.ValAddress, event *types.BatchExecutedEvent) (*types.MsgSubmitEthereumEventResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)

	any, err := codectypes.NewAnyWithValue(event)
	if err != nil {
		return nil, err
	}

	// Add the claim to the store
	_, err = k.RecordEventVote(ctx, event, any)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "create attestation")
	}

	// Emit the handle message event
	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, fmt.Sprintf("%T", event)),
			// TODO: maybe return something better here? is this the right string representation?
			sdk.NewAttribute(types.AttributeKeyAttestationID, string(types.GetEthereumEventVoteRecordKey(event.EventNonce, event.Hash()))),
		),
	)

	return &types.MsgSubmitEthereumEventResponse{}, nil
}

// ERC20Deployed handles MsgERC20Deployed
func (k msgServer) erc20DeployedEvent(c context.Context, signer sdk.ValAddress, event *types.ERC20DeployedEvent) (*types.MsgSubmitEthereumEventResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)


	return &types.MsgSubmitEthereumEventResponse{}, nil
}

// contractCallExecuted handles claims for executing a logic call on Ethereum
func (k msgServer) contractCallExecuted(c context.Context, signer sdk.ValAddress, event *types.ContractCallExecutedEvent) (*types.MsgSubmitEthereumEventResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)

	any, err := codectypes.NewAnyWithValue(event)
	if err != nil {
		return nil, err
	}

	// Add the claim to the store
	_, err = k.RecordEventVote(ctx, event, any)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "create attestation")
	}

	// Emit the handle message event
	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, fmt.Sprintf("%T", event)),
			// TODO: maybe return something better here? is this the right string representation?
			sdk.NewAttribute(types.AttributeKeyAttestationID, string(types.GetEthereumEventVoteRecordKey(event.EventNonce, event.Hash()))),
		),
	)

	return &types.MsgSubmitEthereumEventResponse{}, nil
}

func (k msgServer) CancelSendToEthereum(c context.Context, msg *types.MsgCancelSendToEthereum) (*types.MsgCancelSendToEthereumResponse, error) {
	ctx := sdk.UnwrapSDKContext(c)
	sender, err := sdk.AccAddressFromBech32(msg.Sender)
	if err != nil {
		return nil, err
	}
	err = k.RemoveFromOutgoingPoolAndRefund(ctx, msg.Id, sender)
	if err != nil {
		return nil, err
	}

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			sdk.EventTypeMessage,
			sdk.NewAttribute(sdk.AttributeKeyModule, msg.Type()),
			sdk.NewAttribute(types.AttributeKeyOutgoingTXID, fmt.Sprint(msg.Id)),
		),
	)

	return &types.MsgCancelSendToEthereumResponse{}, nil
}
