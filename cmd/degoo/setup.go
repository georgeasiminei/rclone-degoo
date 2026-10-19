//go:build linux

package degoo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	degoobackend "github.com/rclone/rclone/backend/degoo"
	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/backend/degoo/auth"
	rfs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/spf13/cobra"
)

type folderLocation struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category int    `json:"category"`
}

func transportDefaults() configmap.Simple {
	info, err := rfs.Find("degoo")
	if err != nil {
		panic("Degoo backend is not registered")
	}
	m := configmap.Simple{}
	for _, name := range []string{"api_key", "api_url", "rest_url"} {
		m[name] = fmt.Sprint(info.Options.Get(name).Default)
	}
	return m
}

func setupAPI(ctx context.Context, m configmap.Getter) (*api.Client, error) {
	get := func(name string) string { value, _ := m.Get(name); return value }
	client := fshttp.NewClient(ctx)
	tokens, err := auth.NewWithCredentials(client, get("auth_file"), get("rest_url"), get("credentials_file"))
	if err != nil {
		return nil, err
	}
	apiClient := api.New(client, tokens, get("api_url"), get("api_key"))
	if err := apiClient.SetRateLimitFile(get("auth_file") + ".rate-limit.json"); err != nil {
		return nil, fmt.Errorf("degoo: load provider rate-limit state: %w", err)
	}
	return apiClient, nil
}

func withCredentialsFile(m configmap.Getter, filename string) configmap.Getter {
	if filename == "" {
		return m
	}
	return configmap.New().
		AddGetter(configmap.Simple{"credentials_file": filename}, configmap.PriorityNormal).
		AddGetter(m, configmap.PriorityMax)
}

func writeFolders(out io.Writer, items []api.Item) error {
	entries := make([]folderLocation, 0)
	for _, item := range items {
		// Native directories and device containers are selectable parents.
		if item.Category == 1 || item.Category == 2 || item.Category == 6 {
			entries = append(entries, folderLocation{item.ID, item.Name, item.Category})
		}
	}
	return json.NewEncoder(out).Encode(entries)
}

func addSetupCommands(root *cobra.Command) {
	var parentID string
	var foldersVaultConfig string
	folders := &cobra.Command{Use: "folders auth.json", Short: "List Degoo device containers and folders for vault setup.", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if err := safeLogging(c.Context()); err != nil {
			return err
		}
		m := transportDefaults()
		m["auth_file"] = args[0]
		credentialsFile, err := credentialsFileForAuth(args[0], foldersVaultConfig)
		if err != nil {
			return err
		}
		if credentialsFile != "" {
			m["credentials_file"] = credentialsFile
		}
		client, err := setupAPI(c.Context(), m)
		if err != nil {
			return err
		}
		rfs.Logf(nil, "degoo: listing folders (parent ID %q)", parentID)
		items, err := client.ListDir(c.Context(), parentID)
		if err != nil {
			return err
		}
		rfs.Logf(nil, "degoo: folder query completed (parent ID %q, %d items)", parentID, len(items))
		return writeFolders(c.OutOrStdout(), items)
	}}
	folders.Flags().StringVar(&parentID, "parent-id", "0", "Parent folder ID; 0 lists account device containers")
	folders.Flags().StringVar(&foldersVaultConfig, "vault-config", "", "Vault configuration containing encrypted account credentials")
	var createParent, name string
	var createVaultConfig string
	create := &cobra.Command{Use: "create-vault auth.json", Short: "Create a dedicated vault folder and print its ID.", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if err := safeLogging(c.Context()); err != nil {
			return err
		}
		if createParent == "" || createParent == "0" || strings.TrimSpace(createParent) != createParent {
			return errors.New("--parent-id must identify a device container or folder, not account root 0")
		}
		if !validSetupName(name) {
			return errors.New("--name must be one non-reserved folder name")
		}
		m := transportDefaults()
		m["auth_file"], m["root_folder_id"] = args[0], createParent
		credentialsFile, err := credentialsFileForAuth(args[0], createVaultConfig)
		if err != nil {
			return err
		}
		if credentialsFile != "" {
			m["credentials_file"] = credentialsFile
		}
		base, err := degoobackend.NewFs(c.Context(), "degoo-setup", "", m)
		if err != nil {
			return err
		}
		df := base.(*degoobackend.Fs)
		client, err := setupAPI(c.Context(), m)
		if err != nil {
			return err
		}
		items, err := client.ListDir(c.Context(), createParent)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Name == name {
				return errors.New("the requested name already exists; choose a new dedicated vault name")
			}
		}
		if err := df.Mkdir(c.Context(), name); err != nil {
			return err
		}
		id, found, err := df.FindLeaf(c.Context(), createParent, name)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("created vault could not be resolved; inspect the remote before retrying")
		}
		_, err = fmt.Fprintln(c.OutOrStdout(), id)
		return err
	}}
	create.Flags().StringVar(&createParent, "parent-id", "", "Parent device container or folder ID (required)")
	create.Flags().StringVar(&name, "name", "", "New dedicated vault folder name (required)")
	create.Flags().StringVar(&createVaultConfig, "vault-config", "", "Vault configuration containing encrypted account credentials")
	var rootOnly bool
	location := &cobra.Command{Use: "location vault.json", Short: "Show the configured vault root or its remote ancestor folders.", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if err := safeLogging(c.Context()); err != nil {
			return err
		}
		vault, err := readConfig(args[0])
		if err != nil {
			return err
		}
		info, _, _, m, err := rfs.ConfigFs(vault.Remote)
		if err != nil {
			return err
		}
		if info.Name != "degoo" {
			return errors.New("vault remote must be Degoo")
		}
		client, err := setupAPI(c.Context(), withCredentialsFile(m, args[0]))
		if err != nil {
			return err
		}
		id, _ := m.Get("root_folder_id")
		var entries []folderLocation
		if rootOnly {
			rfs.Logf(nil, "degoo: inspecting configured vault folder (ID %q)", id)
			entries, err = configuredRootFolder(c.Context(), client.GetItem, id)
		} else {
			rfs.Logf(nil, "degoo: inspecting vault ancestry (configured folder ID %q)", id)
			entries, err = ancestorFolders(c.Context(), client.GetItem, id)
		}
		if err != nil {
			return err
		}
		if rootOnly {
			rfs.Logf(nil, "degoo: configured vault folder resolved (ID %q)", id)
		} else {
			rfs.Logf(nil, "degoo: vault ancestry resolved (configured folder ID %q, %d folders)", id, len(entries))
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(entries)
	}}
	location.Flags().BoolVar(&rootOnly, "root-only", false, "Inspect only the configured vault folder; do not request its ancestors")
	root.AddCommand(folders, create, location, newCheckVaultCommand(setupAPI))
}

func validSetupName(name string) bool {
	return name != "" && name != "." && name != ".." && strings.TrimSpace(name) == name && !strings.ContainsAny(name, "/\\\x00") && !strings.HasPrefix(name, ".rclone-degoo-")
}

func ancestorFolders(ctx context.Context, get func(context.Context, string) (api.Item, error), id string) ([]folderLocation, error) {
	entries := make([]folderLocation, 0)
	seen := make(map[string]bool)
	for id != "" && id != "0" {
		if seen[id] || len(entries) >= 128 {
			return nil, errors.New("degoo returned a cyclic or excessively deep folder ancestry")
		}
		seen[id] = true
		item, err := get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("inspect ancestor folder ID %q: %w", id, err)
		}
		if item.ID != id {
			return nil, errors.New("degoo returned mismatched folder metadata")
		}
		entries = append(entries, folderLocation{item.ID, item.Name, item.Category})
		id = item.ParentID
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return entries, nil
}

func configuredRootFolder(ctx context.Context, get func(context.Context, string) (api.Item, error), id string) ([]folderLocation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) != id || id == "" || id == "0" {
		return nil, errors.New("configured root_folder_id must identify a dedicated vault folder")
	}
	item, err := get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("inspect root folder ID %q: %w", id, err)
	}
	if item.ID != id {
		return nil, errors.New("degoo returned mismatched folder metadata")
	}
	return []folderLocation{{item.ID, item.Name, item.Category}}, nil
}
