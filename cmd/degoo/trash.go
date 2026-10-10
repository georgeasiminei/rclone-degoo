//go:build linux

package degoo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	rfs "github.com/rclone/rclone/fs"
	"github.com/spf13/cobra"
)

type trashEntry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ParentID     string `json:"parent_id"`
	Category     int    `json:"category"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	Created      string `json:"created,omitempty"`
	Modified     string `json:"modified,omitempty"`
	InRecycleBin bool   `json:"in_recycle_bin"`
}

type trashJSON struct {
	Items      []trashEntry `json:"items"`
	Count      int          `json:"count"`
	TotalBytes int64        `json:"total_bytes"`
}

func addTrashCommands(root *cobra.Command) {
	var limit int
	var jsonOutput bool
	trash := &cobra.Command{
		Use:   "trash auth.json",
		Short: "List items in the Degoo account recycle bin.",
		Long: `List the account-wide Degoo recycle bin.

Trash is not a path in the vault or a normal rclone remote. This command is
read-only and follows all provider pagination pages. The default limit is zero,
which lists every trash item. No restore operation is currently known.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := safeLogging(c.Context()); err != nil {
				return err
			}
			client, err := trashAPI(c.Context(), args[0])
			if err != nil {
				return err
			}
			items, err := client.ListTrash(c.Context(), limit)
			if err != nil {
				return err
			}
			return writeTrash(c.OutOrStdout(), items, jsonOutput)
		},
	}
	trash.Flags().IntVar(&limit, "limit", 0, "Maximum number of trash items to return (0 means all)")
	trash.Flags().BoolVar(&jsonOutput, "json", false, "Output trash items as JSON")

	var yes bool
	var dryRun bool
	empty := &cobra.Command{
		Use:   "empty-trash auth.json",
		Short: "Permanently delete every item in the Degoo account recycle bin.",
		Long: `Permanently delete every item in the account-wide Degoo recycle bin.

This operation is irreversible. It lists all trash items and then requests one
bulk permanent deletion. Use --dry-run to inspect the items first. Use --yes
only when you are certain. Restore from trash is not currently supported.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := safeLogging(c.Context()); err != nil {
				return err
			}
			client, err := trashAPI(c.Context(), args[0])
			if err != nil {
				return err
			}
			items, err := client.ListTrash(c.Context(), 0)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				_, err = fmt.Fprintln(c.OutOrStdout(), "Degoo trash is empty.")
				return err
			}
			if dryRun {
				return writeTrash(c.OutOrStdout(), items, false)
			}
			if !yes {
				if err := confirmEmptyTrash(c, items); err != nil {
					return err
				}
			}
			if err := client.DeleteTrashItems(c.Context(), itemIDs(items)); err != nil {
				return fmt.Errorf("degoo: empty trash: %w", err)
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Permanently deleted %d item(s), %s from Degoo trash.\n", len(items), rfs.SizeSuffix(trashTotal(items)).ByteUnit())
			return err
		},
	}
	empty.Flags().BoolVar(&yes, "yes", false, "Skip the two interactive confirmations")
	empty.Flags().BoolVar(&dryRun, "dry-run", false, "List the items that would be deleted without changing them")

	root.AddCommand(trash, empty)
}

func trashAPI(ctx context.Context, authFile string) (*api.Client, error) {
	m := transportDefaults()
	m["auth_file"] = authFile
	return setupAPI(ctx, m)
}

func writeTrash(out io.Writer, items []api.Item, jsonOutput bool) error {
	if jsonOutput {
		entries := make([]trashEntry, 0, len(items))
		for _, item := range items {
			entries = append(entries, trashEntry{
				ID:           item.ID,
				Name:         item.Name,
				ParentID:     item.ParentID,
				Category:     item.Category,
				Type:         trashCategoryName(item.Category),
				Size:         item.Size,
				Created:      formatTrashTime(item.CreationTime),
				Modified:     formatTrashTime(item.LastModificationTime),
				InRecycleBin: item.IsInRecycleBin,
			})
		}
		return json.NewEncoder(out).Encode(trashJSON{
			Items:      entries,
			Count:      len(entries),
			TotalBytes: trashTotal(items),
		})
	}
	if len(items) == 0 {
		_, err := fmt.Fprintln(out, "Degoo trash is empty.")
		return err
	}
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ID\tTYPE\tSIZE\tNAME"); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", item.ID, trashCategoryName(item.Category), rfs.SizeSuffix(item.Size).ByteUnit(), item.Name); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "%d item(s), %s total\n", len(items), rfs.SizeSuffix(trashTotal(items)).ByteUnit())
	return err
}

func confirmEmptyTrash(c *cobra.Command, items []api.Item) error {
	total := trashTotal(items)
	if _, err := fmt.Fprintf(c.OutOrStdout(), "WARNING: this will permanently delete %d item(s), %s from the Degoo account-wide recycle bin.\n", len(items), rfs.SizeSuffix(total).ByteUnit()); err != nil {
		return err
	}
	if _, err := fmt.Fprint(c.OutOrStdout(), "Type YES to continue: "); err != nil {
		return err
	}
	reader := bufio.NewReader(c.InOrStdin())
	first, err := readConfirmation(reader)
	if err != nil {
		return err
	}
	if first != "YES" {
		return errors.New("aborted by user")
	}
	if _, err := fmt.Fprintf(c.OutOrStdout(), "FINAL WARNING: %d item(s) will be gone forever.\n", len(items)); err != nil {
		return err
	}
	if _, err := fmt.Fprint(c.OutOrStdout(), "Type EMPTY TRASH to continue: "); err != nil {
		return err
	}
	second, err := readConfirmation(reader)
	if err != nil {
		return err
	}
	if second != "EMPTY TRASH" {
		return errors.New("aborted by user")
	}
	return nil
}

func readConfirmation(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.ToUpper(strings.TrimSpace(line)), nil
}

func trashCategoryName(category int) string {
	switch category {
	case 0:
		return "file"
	case 1:
		return "device"
	case 2:
		return "folder"
	case 3:
		return "image"
	case 4:
		return "video"
	case 5:
		return "music"
	case 6:
		return "document"
	case 10:
		return "recycle-bin"
	default:
		return "unknown"
	}
}

func itemIDs(items []api.Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func trashTotal(items []api.Item) int64 {
	var total int64
	for _, item := range items {
		total += item.Size
	}
	return total
}

func formatTrashTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}
