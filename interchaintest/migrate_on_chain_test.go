package interchaintest

import (
	"context"
	"os"
	"testing"

	"cosmossdk.io/math"
	"github.com/go-resty/resty/v2"
	"github.com/jarcoal/httpmock"
	"github.com/spf13/cobra"
	"github.com/strangelove-ventures/interchaintest/v8"
	"github.com/strangelove-ventures/interchaintest/v8/ibc"
	"github.com/stretchr/testify/require"

	"github.com/liftedinit/mfx-migrator/internal/store"

	"github.com/liftedinit/mfx-migrator/cmd"
	"github.com/liftedinit/mfx-migrator/testutils"
)

type Amounts struct {
	Old math.Int
	New math.Int
}

type Expected struct {
	Bank Amounts
	User Amounts
}

func TestMigrateOnChain(t *testing.T) {
	ctx := context.Background()
	tmpdir := interchaintest.TempDir(t)
	if err := os.Chdir(tmpdir); err != nil {
		t.Fatal(err)
	}

	// Set up the chain and keyring
	appChain, bankAcc, gasStationAcc := SetupChain(t, ctx)
	chainConfig := appChain.Config()
	err := SetupKeyring(tmpdir, []ibc.Wallet{bankAcc, gasStationAcc})
	require.NoError(t, err)

	grants, err := appChain.FeeGrantQueryAllowances(ctx, bankAcc.FormattedAddress())
	require.NoError(t, err)
	require.Len(t, grants, 1)

	// Prepare the migrate command
	command := &cobra.Command{Use: "migrate", PersistentPreRunE: cmd.RootCmdPersistentPreRunE, RunE: cmd.MigrateCmdRunE}
	cmd.SetupRootCmdFlags(command)
	cmd.SetupMigrateCmdFlags(command)

	// Create a new resty client and inject it into the command context
	rClient := resty.New()
	cCtx := context.WithValue(context.Background(), cmd.RestyClientKey, rClient)
	command.SetContext(cCtx)

	// Enable http mocking on the resty client
	httpmock.ActivateNonDefault(rClient.GetClient())
	defer httpmock.DeactivateAndReset()

	slice := []string{
		"--url", testutils.RootUrl,
		"--uuid", testutils.Uuid,
		"--username", "user",
		"--password", "pass",
		"--chain-id", chainConfig.ChainID,
		"--address-prefix", chainConfig.Bech32Prefix,
		"--node-address", appChain.GetHostRPCAddress(),
		"--keyring-backend", "test",
		"--bank-address", bankAcc.KeyName(),
		"--chain-home", tmpdir,
		"--gas-price", "0.0011",
		"--fee-granter", gasStationAcc.FormattedAddress(),
		"--binary", "manifestd",
	}

	defaultGenesisAmtPlusThousand := DefaultGenesisAmt.Add(math.NewInt(1000)) // Genesis amount + 1000
	defaultGenesisAmtMinThousand := DefaultGenesisAmt.Sub(math.NewInt(1000))  // Genesis amount - 1000

	tt := []struct {
		name      string
		args      []string
		err       string
		expected  Expected
		endpoints []testutils.HttpResponder
	}{
		{name: "1:1000 tokens", args: slice, endpoints: []testutils.HttpResponder{
			{Method: "POST", Url: testutils.LoginUrl, Responder: testutils.AuthResponder},
			{Method: "GET", Url: "=~^" + testutils.DefaultMigrationUrl, Responder: testutils.MustMigrationGetResponder(store.CLAIMED)},
			{Method: "GET", Url: "=~^" + testutils.DefaultTransactionUrl, Responder: testutils.MustNewLedgerSendTransactionResponseResponder("1000")},
			{Method: "PUT", Url: "=~^" + testutils.DefaultMigrationUrl, Responder: testutils.MigrationUpdateResponder},
		}, expected: Expected{
			Bank: Amounts{Old: DefaultGenesisAmt, New: defaultGenesisAmtMinThousand},
			User: Amounts{Old: math.ZeroInt(), New: math.NewInt(1000)},
		}},
		{name: "insufficient funds", args: slice, endpoints: []testutils.HttpResponder{
			{Method: "POST", Url: testutils.LoginUrl, Responder: testutils.AuthResponder},
			{Method: "GET", Url: "=~^" + testutils.DefaultMigrationUrl, Responder: testutils.MustMigrationGetResponder(store.CLAIMED)},
			{Method: "GET", Url: "=~^" + testutils.DefaultTransactionUrl, Responder: testutils.MustNewLedgerSendTransactionResponseResponder(defaultGenesisAmtPlusThousand.String())},
			{Method: "PUT", Url: "=~^" + testutils.DefaultMigrationUrl, Responder: testutils.MigrationUpdateResponder},
		}, expected: Expected{
			Bank: Amounts{Old: defaultGenesisAmtMinThousand},
			User: Amounts{Old: math.NewInt(1000)},
		}, err: "insufficient funds"},
		{name: "invalid coins", args: slice, endpoints: []testutils.HttpResponder{
			{Method: "POST", Url: testutils.LoginUrl, Responder: testutils.AuthResponder},
			{Method: "GET", Url: "=~^" + testutils.DefaultMigrationUrl, Responder: testutils.MustMigrationGetResponder(store.CLAIMED)},
			{Method: "GET", Url: "=~^" + testutils.DefaultTransactionUrl, Responder: testutils.MustNewLedgerSendTransactionResponseResponder("0")},
			{Method: "PUT", Url: "=~^" + testutils.DefaultMigrationUrl, Responder: testutils.MigrationUpdateResponder},
		}, expected: Expected{
			Bank: Amounts{Old: defaultGenesisAmtMinThousand},
			User: Amounts{Old: math.NewInt(1000)},
		}, err: "invalid coins"},
		{name: "all tokens from bank", args: slice, endpoints: []testutils.HttpResponder{
			{Method: "POST", Url: testutils.LoginUrl, Responder: testutils.AuthResponder},
			{Method: "GET", Url: "=~^" + testutils.DefaultMigrationUrl, Responder: testutils.MustMigrationGetResponder(store.CLAIMED)},
			{Method: "GET", Url: "=~^" + testutils.DefaultTransactionUrl, Responder: testutils.MustNewMultisigTransactionResponseResponder(defaultGenesisAmtMinThousand.String())},
			{Method: "PUT", Url: "=~^" + testutils.DefaultMigrationUrl, Responder: testutils.MigrationUpdateResponder},
		}, expected: Expected{
			Bank: Amounts{Old: defaultGenesisAmtMinThousand, New: math.ZeroInt()},
			User: Amounts{Old: math.NewInt(1000), New: DefaultGenesisAmt},
		}},
	}

	for _, tc := range tt {
		// Set up the work item
		testutils.SetupWorkItem(t)
		workItemPath := tmpdir + "/" + testutils.Uuid
		workItemPathJson := workItemPath + ".json"

		t.Run(tc.name, func(t *testing.T) {
			// Register the http mock responders
			for _, endpoint := range tc.endpoints {
				httpmock.RegisterResponder(endpoint.Method, endpoint.Url, endpoint.Responder)
			}

			// Check the balance of the bank account pre-migration
			balanceBO, err := appChain.BankQueryBalance(ctx, bankAcc.FormattedAddress(), Denom)
			require.NoError(t, err)
			require.Equal(t, balanceBO, tc.expected.Bank.Old)

			// Check the balance of the manifest destination account pre-migration
			balanceUO, err := appChain.BankQueryBalance(ctx, testutils.ManifestAddress, Denom)
			require.NoError(t, err)
			require.Equal(t, balanceUO, tc.expected.User.Old)

			// Execute the migration
			_, err = testutils.Execute(t, command, tc.args...)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)

				// Check the status of the local work item
				item, err := store.LoadState(workItemPath)
				require.NoError(t, err)
				require.Equal(t, item.Status, store.FAILED)
				require.Contains(t, *item.Error, tc.err)
			} else {
				require.NoError(t, err)

				// Check the balance of the bank account post-migration
				balanceBN, err := appChain.BankQueryBalance(ctx, bankAcc.FormattedAddress(), Denom)
				require.NoError(t, err)
				require.Equal(t, balanceBN, tc.expected.Bank.New)

				// Check the balance of the manifest destination account post-migration
				balanceUN, err := appChain.BankQueryBalance(ctx, testutils.ManifestAddress, Denom)
				require.NoError(t, err)
				require.Equal(t, balanceUN, tc.expected.User.New)
			}
			httpmock.Reset()
		})

		// Remove the work item file if it exists
		_, err = os.Stat(workItemPathJson)
		if !os.IsNotExist(err) {
			err = os.Remove(workItemPathJson)
			require.NoError(t, err)
		}
	}
}
