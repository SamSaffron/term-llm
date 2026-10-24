package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/spf13/cobra"
)

var containPeersStateDir string
var containPeersCmd = &cobra.Command{Use: "peers", Short: "List or revoke local paired-device grants", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
	store, err := openContainPeerGrants()
	if err != nil {
		return err
	}
	grants, err := store.List()
	if err != nil {
		return err
	}
	for _, g := range grants {
		status := "active"
		if err := g.Check(time.Now()); err != nil {
			status = "inactive"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%q\t%s\t%s\n", g.ID, status, g.DeviceName, g.DeviceFingerprint(), g.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}}
var containPeersRevokeCmd = &cobra.Command{Use: "revoke <grant-id>", Short: "Revoke a device grant (active peer closes within one second)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	raw, err := pairing.DecodeB64(args[0])
	if err != nil || len(raw) != pairing.GrantIDSize {
		return fmt.Errorf("invalid peer grant ID")
	}
	store, err := openContainPeerGrants()
	if err != nil {
		return err
	}
	if err = store.Revoke(args[0], time.Now()); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Device grant revoked; new requests fail immediately and the active peer closes within one second.")
	return nil
}}

func openContainPeerGrants() (*pairing.GrantStore, error) {
	dir := containPeersStateDir
	if dir == "" {
		nodes, err := apphub.DefaultNodesFile()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(filepath.Dir(nodes), "peer")
	}
	return pairing.OpenGrantStore(filepath.Join(dir, "device-grants.json"))
}
func init() {
	containCmd.AddCommand(containPeersCmd)
	containPeersCmd.AddCommand(containPeersRevokeCmd)
	containPeersCmd.PersistentFlags().StringVar(&containPeersStateDir, "state-dir", "", "Private peer state directory (same as contain pair --peer-state-dir)")
}
