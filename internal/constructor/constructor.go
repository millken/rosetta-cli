package constructor

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"time"

	"github.com/coinbase/rosetta-cli/configuration"
	"github.com/coinbase/rosetta-cli/internal/scenario"
	"github.com/coinbase/rosetta-cli/internal/utils"

	"github.com/coinbase/rosetta-sdk-go/keys"
	"github.com/coinbase/rosetta-sdk-go/types"
	"github.com/fatih/color"
	"github.com/jinzhu/copier"
)

func init() {
	rand.Seed(time.Now().UTC().UnixNano())
}

// action is some supported intent we can
// perform. This is used to determine the minimum
// required balance to complete the transaction.
type action string

const (
	// defaultSleepTime is the default time we sleep
	// while waiting to perform the next task.
	defaultSleepTime = 10

	// newAccountSend is a send to a new account.
	newAccountSend action = "new-account-send"

	// ExistingAccountSend is a send to an existing account.
	existingAccountSend action = "existing-account-send"

	// changeSend is a send that creates a UTXO
	// for the recipient and sends the remainder
	// to a change UTXO.
	changeSend action = "change-send"

	// fullSend is a send that transfers
	// all value in one UTXO into another.
	fullSend action = "full-send"
)

var (
	// ErrInsufficientFunds is returned when we must
	// request funds.
	ErrInsufficientFunds = errors.New("insufficient funds")
)

type ConstructorHelper interface {
	Derive(
		context.Context,
		*types.NetworkIdentifier,
		*types.PublicKey,
		map[string]interface{},
	) (string, map[string]interface{}, error)

	Preprocess(
		context.Context,
		*types.NetworkIdentifier,
		[]*types.Operation,
		map[string]interface{},
	) (map[string]interface{}, error)

	Metadata(
		context.Context,
		*types.NetworkIdentifier,
		map[string]interface{},
	) (map[string]interface{}, error)

	Payloads(
		context.Context,
		*types.NetworkIdentifier,
		[]*types.Operation,
		map[string]interface{},
	) (string, []*types.SigningPayload, error)

	Parse(
		context.Context,
		*types.NetworkIdentifier,
		bool, // signed
		string, // transaction
	) ([]*types.Operation, []string, map[string]interface{}, error)

	Combine(
		context.Context,
		*types.NetworkIdentifier,
		string, // unsigned transaction
		[]*types.Signature,
	) (string, error)

	Hash(
		context.Context,
		*types.NetworkIdentifier,
		string, // network transaction
	) (*types.TransactionIdentifier, error)

	ExpectedOperations(
		[]*types.Operation, // intent
		[]*types.Operation, // observed
		bool, // error extra
		bool, // confirm success
	) error

	ExpectedSigners(
		[]*types.SigningPayload,
		[]string,
	) error

	Sign(
		context.Context,
		[]*types.SigningPayload,
	) ([]*types.Signature, error)

	StoreKey(
		context.Context,
		string,
		*keys.KeyPair,
	) error

	AccountBalance(
		context.Context,
		*types.AccountIdentifier,
		*types.Currency,
	) (*big.Int, error)

	CoinBalance(
		context.Context,
		*types.AccountIdentifier,
		*types.Currency,
	) (*big.Int, *types.CoinIdentifier, error)

	LockedAddresses(context.Context) ([]string, error)

	AllAddresses(ctx context.Context) ([]string, error)
}

type ConstructorHandler interface {
	AddressCreated(context.Context, string) error
}

type Constructor struct {
	network         *types.NetworkIdentifier
	accountingModel configuration.AccountingModel
	currency        *types.Currency
	minimumBalance  *big.Int
	maximumFee      *big.Int

	helper  ConstructorHelper
	handler ConstructorHandler
}

// CreateTransaction constructs and signs a transaction with the provided intent.
func (c *Constructor) CreateTransaction(
	ctx context.Context,
	intent []*types.Operation,
) (*types.TransactionIdentifier, string, error) {
	metadataRequest, err := c.helper.Preprocess(
		ctx,
		c.network,
		intent,
		nil,
	)
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to preprocess", err)
	}

	requiredMetadata, err := c.helper.Metadata(
		ctx,
		c.network,
		metadataRequest,
	)
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to construct metadata", err)
	}

	unsignedTransaction, payloads, err := c.helper.Payloads(
		ctx,
		c.network,
		intent,
		requiredMetadata,
	)
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to construct payloads", err)
	}

	parsedOps, signers, _, err := c.helper.Parse(
		ctx,
		c.network,
		false,
		unsignedTransaction,
	)
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to parse unsigned transaction", err)
	}

	if len(signers) != 0 {
		return nil, "", fmt.Errorf("signers should be empty in unsigned transaction but found %d", len(signers))
	}

	if err := c.helper.ExpectedOperations(intent, parsedOps, false, false); err != nil {
		return nil, "", fmt.Errorf("%w: unsigned parsed ops do not match intent", err)
	}

	signatures, err := c.helper.Sign(ctx, payloads)
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to sign payloads", err)
	}

	networkTransaction, err := c.helper.Combine(
		ctx,
		c.network,
		unsignedTransaction,
		signatures,
	)
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to combine signatures", err)
	}

	signedParsedOps, signers, _, err := c.helper.Parse(
		ctx,
		c.network,
		true,
		networkTransaction,
	)
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to parse signed transaction", err)
	}

	if err := c.helper.ExpectedOperations(intent, signedParsedOps, false, false); err != nil {
		return nil, "", fmt.Errorf("%w: signed parsed ops do not match intent", err)
	}

	if err := c.helper.ExpectedSigners(payloads, signers); err != nil {
		return nil, "", fmt.Errorf("%w: signed transactions signers do not match intent", err)
	}

	transactionIdentifier, err := c.helper.Hash(
		ctx,
		c.network,
		networkTransaction,
	)
	if err != nil {
		return nil, "", fmt.Errorf("%w: unable to get transaction hash", err)
	}

	return transactionIdentifier, networkTransaction, nil
}

// NewAddress generates a new keypair and
// derives its address offline. This only works
// for blockchains that don't require an on-chain
// action to create an account.
func (c *Constructor) NewAddress(ctx context.Context, curveType types.CurveType) (string, error) {
	kp, err := keys.GenerateKeypair(curveType)
	if err != nil {
		return "", fmt.Errorf("%w unable to generate keypair", err)
	}

	address, _, err := c.helper.Derive(
		ctx,
		c.network,
		kp.PublicKey,
		nil,
	)

	if err != nil {
		return "", fmt.Errorf("%w: unable to derive address", err)
	}

	err = c.helper.StoreKey(ctx, address, kp)
	if err != nil {
		return "", fmt.Errorf("%w: unable to store address", err)
	}

	if c.handler.AddressCreated(ctx, address); err != nil {
		return "", fmt.Errorf("%w: could not handle address creation", err)
	}

	return address, nil
}

// requestFunds prompts the user to load
// a particular address with funds from a faucet.
// TODO: automate this using an API faucet.
func (c *Constructor) requestFunds(
	ctx context.Context,
	address string,
) (*big.Int, *types.CoinIdentifier, error) {
	printedMessage := false
	for ctx.Err() == nil {
		balance, coinIdentifier, err := c.balance(ctx, address)
		if err != nil {
			return nil, nil, err
		}

		minBalance := c.minimumRequiredBalance(newAccountSend)
		if c.accountingModel == configuration.UtxoModel {
			minBalance = c.minimumRequiredBalance(changeSend)
		}

		if balance != nil && new(big.Int).Sub(balance, minBalance).Sign() != -1 {
			color.Green("Found balance %s on %s", utils.PrettyAmount(balance, c.currency), address)
			return balance, coinIdentifier, nil
		}

		if !printedMessage {
			color.Yellow("Waiting for funds on %s", address)
			printedMessage = true
		}
		time.Sleep(defaultSleepTime * time.Second)
	}

	return nil, nil, ctx.Err()
}

func (c *Constructor) minimumRequiredBalance(action action) *big.Int {
	doubleMinimumBalance := new(big.Int).Add(c.minimumBalance, c.minimumBalance)
	switch action {
	case newAccountSend, changeSend:
		// In this account case, we must have keep a balance above
		// the minimum_balance in the sender's account and send
		// an amount of at least the minimum_balance to the recipient.
		//
		// In the UTXO case, we must send at least the minimum
		// balance to the recipient and the change address (or
		// we will create dust).
		return new(big.Int).Add(doubleMinimumBalance, c.maximumFee)
	case existingAccountSend, fullSend:
		// In the account case, we must keep a balance above
		// the minimum_balance in the sender's account.
		//
		// In the UTXO case, we must send at least the minimum
		// balance to the new UTXO.
		return new(big.Int).Add(c.minimumBalance, c.maximumFee)
	}

	return nil
}

// balance returns the total balance to use for
// a transfer. In the case of a UTXO-based chain,
// this is the largest remaining UTXO.
func (c *Constructor) balance(
	ctx context.Context,
	address string,
) (*big.Int, *types.CoinIdentifier, error) {
	accountIdentifier := &types.AccountIdentifier{Address: address}

	switch c.accountingModel {
	case configuration.AccountModel:
		bal, err := c.helper.AccountBalance(ctx, accountIdentifier, c.currency)

		return bal, nil, err
	case configuration.UtxoModel:
		return c.helper.CoinBalance(ctx, accountIdentifier, c.currency)
	}

	return nil, nil, fmt.Errorf("unable to find balance for %s", address)
}

func (c *Constructor) getBestUnlockedSender(
	ctx context.Context,
	addresses []string,
) (
	string, // best address
	*big.Int, // best balance
	*types.CoinIdentifier, // best coin
	error,
) {
	unlockedAddresses := []string{}
	lockedAddresses, err := c.helper.LockedAddresses(ctx)
	if err != nil {
		return "", nil, nil, fmt.Errorf("%w: unable to get locked addresses", err)
	}

	// Convert to a map so can do fast lookups
	lockedSet := map[string]struct{}{}
	for _, address := range lockedAddresses {
		lockedSet[address] = struct{}{}
	}

	for _, address := range addresses {
		if _, exists := lockedSet[address]; !exists {
			unlockedAddresses = append(unlockedAddresses, address)
		}
	}

	// Only check addresses not currently locked
	var bestAddress string
	var bestBalance *big.Int
	var bestCoin *types.CoinIdentifier
	for _, address := range unlockedAddresses {
		balance, coinIdentifier, err := c.balance(ctx, address)
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: unable to get balance for %s", err, address)
		}

		if bestBalance == nil || new(big.Int).Sub(bestBalance, balance).Sign() == -1 {
			bestAddress = address
			bestBalance = balance
			bestCoin = coinIdentifier
		}
	}

	return bestAddress, bestBalance, bestCoin, nil
}

// findSender fetches all available addresses,
// all locked addresses, and all address balances
// to determine which addresses can facilitate
// a transfer. The sender with the highest
// balance is returned (or the largest UTXO).
func (c *Constructor) findSender(
	ctx context.Context,
) (
	string, // sender
	*big.Int, // balance
	*types.CoinIdentifier, // coin
	error,
) {
	for ctx.Err() == nil {
		addresses, err := c.helper.AllAddresses(ctx)
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: unable to get addresses", err)
		}

		if len(addresses) == 0 { // create new and load
			err := t.generateNewAndRequest(ctx)
			if err != nil {
				return "", nil, nil, fmt.Errorf("%w: unable to generate new and request", err)
			}

			continue // we will exit on next loop
		}

		bestAddress, bestBalance, bestCoin, err := t.getBestUnlockedSender(ctx, addresses)
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: unable to get best unlocked sender", err)
		}

		if len(bestAddress) > 0 {
			return bestAddress, bestBalance, bestCoin, nil
		}

		broadcasts, err := t.broadcastStorage.GetAllBroadcasts(ctx)
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: unable to get broadcasts", err)
		}

		if len(broadcasts) > 0 {
			// This condition occurs when we are waiting for some
			// pending broadcast to complete before creating more
			// transactions.

			time.Sleep(defaultSleepTime * time.Second)
			continue
		}

		if err := t.generateNewAndRequest(ctx); err != nil {
			return "", nil, nil, fmt.Errorf("%w: generate new address and request", err)
		}
	}

	return "", nil, nil, ctx.Err()
}

// findRecipients returns all possible
// recipients (address != sender).
func (c *Constructor) findRecipients(
	ctx context.Context,
	sender string,
) (
	[]string, // recipients with minimum balance
	[]string, // recipients without minimum balance
	error,
) {
	minimumRecipients := []string{}
	belowMinimumRecipients := []string{}

	addresses, err := t.keyStorage.GetAllAddresses(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unable to get address", err)
	}
	for _, a := range addresses {
		if a == sender {
			continue
		}

		// Sending UTXOs always requires sending to the minimum.
		if t.config.Construction.AccountingModel == configuration.UtxoModel {
			belowMinimumRecipients = append(belowMinimumRecipients, a)

			continue
		}

		bal, _, err := t.balance(ctx, a)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: unable to retrieve balance for %s", err, a)
		}

		if new(big.Int).Sub(bal, t.minimumBalance).Sign() >= 0 {
			minimumRecipients = append(minimumRecipients, a)

			continue
		}

		belowMinimumRecipients = append(belowMinimumRecipients, a)
	}

	return minimumRecipients, belowMinimumRecipients, nil
}

// createScenarioContext creates the context to use
// for scenario population.
func (c *Constructor) createScenarioContext(
	sender string,
	senderValue *big.Int,
	recipient string,
	recipientValue *big.Int,
	changeAddress string,
	changeValue *big.Int,
	coinIdentifier *types.CoinIdentifier,
) (*scenario.Context, []*types.Operation, error) {
	// We create a deep copy of the scenaerio (and the change scenario)
	// to ensure we don't accidentally overwrite the loaded configuration
	// while hydrating values.
	scenarioOps := []*types.Operation{}
	if err := copier.Copy(&scenarioOps, t.config.Construction.Scenario); err != nil {
		return nil, nil, fmt.Errorf("%w: unable to copy scenario", err)
	}

	if len(changeAddress) > 0 {
		changeCopy := types.Operation{}
		if err := copier.Copy(&changeCopy, t.config.Construction.ChangeScenario); err != nil {
			return nil, nil, fmt.Errorf("%w: unable to copy change intent", err)
		}

		scenarioOps = append(scenarioOps, &changeCopy)
	}

	return &scenario.Context{
		Sender:         sender,
		SenderValue:    senderValue,
		Recipient:      recipient,
		RecipientValue: recipientValue,
		Currency:       t.config.Construction.Currency,
		CoinIdentifier: coinIdentifier,
		ChangeAddress:  changeAddress,
		ChangeValue:    changeValue,
	}, scenarioOps, nil
}

func (c *Constructor) canGetNewAddress(
	ctx context.Context,
	recipients []string,
) (string, bool, error) {
	availableAddresses, err := t.keyStorage.GetAllAddresses(ctx)
	if err != nil {
		return "", false, fmt.Errorf("%w: unable to get available addresses", err)
	}

	if (rand.Float64() > t.config.Construction.NewAccountProbability &&
		len(availableAddresses) < t.config.Construction.MaxAddresses) || len(recipients) == 0 {
		addr, err := t.newAddress(ctx)
		if err != nil {
			return "", false, fmt.Errorf("%w: cannot create new address", err)
		}

		return addr, true, nil
	}

	return recipients[0], false, nil
}

func (c *Constructor) generateAccountScenario(
	ctx context.Context,
	sender string,
	balance *big.Int,
	minimumRecipients []string,
	belowMinimumRecipients []string,
) (
	*scenario.Context,
	[]*types.Operation, // scenario operations
	error, // ErrInsufficientFunds
) {
	adjustedBalance := new(big.Int).Sub(balance, t.minimumBalance)

	// should send to new account, existing account, or no acccount?
	if new(big.Int).Sub(balance, t.minimumRequiredBalance(newAccountSend)).Sign() != -1 {
		recipient, created, err := t.canGetNewAddress(
			ctx,
			append(minimumRecipients, belowMinimumRecipients...),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: unable to get recipient", err)
		}

		if created || utils.ContainsString(belowMinimumRecipients, recipient) {
			recipientValue := utils.RandomNumber(t.minimumBalance, adjustedBalance)
			return t.createScenarioContext(
				sender,
				recipientValue,
				recipient,
				recipientValue,
				"",
				nil,
				nil,
			)
		}

		// We do not need to send the minimum amount here because the recipient
		// already has a minimum balance.
		recipientValue := utils.RandomNumber(big.NewInt(0), adjustedBalance)
		return t.createScenarioContext(
			sender,
			recipientValue,
			recipient,
			recipientValue,
			"",
			nil,
			nil,
		)
	}

	recipientValue := utils.RandomNumber(big.NewInt(0), adjustedBalance)
	if new(big.Int).Sub(balance, t.minimumRequiredBalance(existingAccountSend)).Sign() != -1 {
		if len(minimumRecipients) == 0 {
			return nil, nil, ErrInsufficientFunds
		}

		return t.createScenarioContext(
			sender,
			recipientValue,
			minimumRecipients[0],
			recipientValue,
			"",
			nil,
			nil,
		)
	}

	// Cannot perform any transfer.
	return nil, nil, ErrInsufficientFunds
}

func (c *Constructor) generateUtxoScenario(
	ctx context.Context,
	sender string,
	balance *big.Int,
	recipients []string,
	coinIdentifier *types.CoinIdentifier,
) (
	*scenario.Context,
	[]*types.Operation, // scenario operations
	error, // ErrInsufficientFunds
) {
	feeLessBalance := new(big.Int).Sub(balance, t.maximumFee)
	recipient, created, err := t.canGetNewAddress(ctx, recipients)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unable to get recipient", err)
	}

	// Need to remove from recipients if did not create a recipient address
	if !created {
		newRecipients := []string{}
		for _, r := range recipients {
			if recipient != r {
				newRecipients = append(newRecipients, r)
			}
		}

		recipients = newRecipients
	}

	// should send to change, no change, or no send?
	if new(big.Int).Sub(balance, t.minimumRequiredBalance(changeSend)).Sign() != -1 &&
		t.config.Construction.ChangeScenario != nil {
		changeAddress, _, err := t.canGetNewAddress(ctx, recipients)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: unable to get change address", err)
		}

		doubleMinimumBalance := new(big.Int).Add(t.minimumBalance, t.minimumBalance)
		changeDifferential := new(big.Int).Sub(feeLessBalance, doubleMinimumBalance)

		recipientShare := utils.RandomNumber(big.NewInt(0), changeDifferential)
		changeShare := new(big.Int).Sub(changeDifferential, recipientShare)

		recipientValue := new(big.Int).Add(t.minimumBalance, recipientShare)
		changeValue := new(big.Int).Add(t.minimumBalance, changeShare)

		return t.createScenarioContext(
			sender,
			balance,
			recipient,
			recipientValue,
			changeAddress,
			changeValue,
			coinIdentifier,
		)
	}

	if new(big.Int).Sub(balance, t.minimumRequiredBalance(fullSend)).Sign() != -1 {
		return t.createScenarioContext(
			sender,
			balance,
			recipient,
			utils.RandomNumber(t.minimumBalance, feeLessBalance),
			"",
			nil,
			nil,
		)
	}

	// Cannot perform any transfer.
	return nil, nil, ErrInsufficientFunds
}

// generateScenario determines what should be done in a given
// transfer based on the sender's balance.
func (c *Constructor) generateScenario(
	ctx context.Context,
	sender string,
	balance *big.Int,
	coinIdentifier *types.CoinIdentifier,
) (
	*scenario.Context,
	[]*types.Operation, // scenario operations
	error, // ErrInsufficientFunds
) {
	minimumRecipients, belowMinimumRecipients, err := t.findRecipients(ctx, sender)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unable to find recipients", err)
	}

	switch t.config.Construction.AccountingModel {
	case configuration.AccountModel:
		return t.generateAccountScenario(
			ctx,
			sender,
			balance,
			minimumRecipients,
			belowMinimumRecipients,
		)
	case configuration.UtxoModel:
		return t.generateUtxoScenario(ctx, sender, balance, belowMinimumRecipients, coinIdentifier)
	}

	return nil, nil, ErrInsufficientFunds
}

func (c *Constructor) generateNewAndRequest(ctx context.Context) error {
	addr, err := t.newAddress(ctx)
	if err != nil {
		return fmt.Errorf("%w: unable to create address", err)
	}

	_, _, err = t.requestFunds(ctx, addr)
	if err != nil {
		return fmt.Errorf("%w: unable to get funds on %s", err, addr)
	}

	return nil
}
