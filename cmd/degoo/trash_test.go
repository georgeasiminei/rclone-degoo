//go:build linux

package degoo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func trashTestItems() []api.Item {
	return []api.Item{
		{ID: "1", Name: "old.txt", ParentID: "parent", Category: 6, Size: 1024, IsInRecycleBin: true},
		{ID: "2", Name: "old.bin", ParentID: "parent", Category: 0, Size: 2048, IsInRecycleBin: true},
	}
}

func TestWriteTrashHumanOutput(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeTrash(&out, trashTestItems(), false))
	output := out.String()
	require.Contains(t, output, "ID")
	require.Contains(t, output, "TYPE")
	require.Contains(t, output, "SIZE")
	require.Contains(t, output, "NAME")
	require.Contains(t, output, "1")
	require.Contains(t, output, "document")
	require.Contains(t, output, "old.txt")
	require.Contains(t, output, "2")
	require.Contains(t, output, "file")
	require.Contains(t, output, "old.bin")
	require.Contains(t, output, "2 item(s)")
	require.Contains(t, output, "3 KiB")
}

func TestWriteTrashJSONOutput(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeTrash(&out, trashTestItems(), true))
	var result trashJSON
	require.NoError(t, json.Unmarshal(out.Bytes(), &result))
	require.Len(t, result.Items, 2)
	require.Equal(t, 2, result.Count)
	require.Equal(t, int64(3072), result.TotalBytes)
	require.Equal(t, "old.txt", result.Items[0].Name)
	require.True(t, result.Items[0].InRecycleBin)
	require.Equal(t, "document", result.Items[0].Type)
	require.Equal(t, "file", result.Items[1].Type)
}

func TestWriteTrashJSONExcludesProviderURL(t *testing.T) {
	items := trashTestItems()
	items[0].URL = "https://cdn.invalid/secret-signed-url"
	var out bytes.Buffer
	require.NoError(t, writeTrash(&out, items, true))
	require.NotContains(t, out.String(), "cdn.invalid")
	require.NotContains(t, out.String(), "secret-signed-url")
}

func TestWriteTrashEmpty(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeTrash(&out, nil, false))
	require.Equal(t, "Degoo trash is empty.\n", out.String())
}

func TestTrashHelpers(t *testing.T) {
	items := trashTestItems()
	require.Equal(t, []string{"1", "2"}, itemIDs(items))
	require.Equal(t, int64(3072), trashTotal(items))
	require.Equal(t, "file", trashCategoryName(0))
	require.Equal(t, "device", trashCategoryName(1))
	require.Equal(t, "folder", trashCategoryName(2))
	require.Equal(t, "image", trashCategoryName(3))
	require.Equal(t, "video", trashCategoryName(4))
	require.Equal(t, "music", trashCategoryName(5))
	require.Equal(t, "document", trashCategoryName(6))
	require.Equal(t, "recycle-bin", trashCategoryName(10))
	require.Equal(t, "unknown", trashCategoryName(99))
}

func TestFormatTrashTime(t *testing.T) {
	require.Empty(t, formatTrashTime(time.Time{}))
	require.Equal(t, "2026-10-10T12:34:56Z", formatTrashTime(time.Date(2026, 10, 10, 12, 34, 56, 0, time.UTC)))
}

func newTrashConfirmationCommand(input string) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(input))
	return command, &out
}

func TestConfirmEmptyTrashSucceeds(t *testing.T) {
	command, out := newTrashConfirmationCommand("yes\nempty trash\n")
	require.NoError(t, confirmEmptyTrash(command, trashTestItems()))
	output := out.String()
	require.Contains(t, output, "WARNING: this will permanently delete 2 item(s)")
	require.Contains(t, output, "Type YES to continue")
	require.Contains(t, output, "FINAL WARNING: 2 item(s) will be gone forever")
	require.Contains(t, output, "Type EMPTY TRASH to continue")
}

func TestConfirmEmptyTrashRejectsWrongFirstAnswer(t *testing.T) {
	command, out := newTrashConfirmationCommand("no\nempty trash\n")
	err := confirmEmptyTrash(command, trashTestItems())
	require.EqualError(t, err, "aborted by user")
	require.Contains(t, out.String(), "Type YES to continue")
	require.NotContains(t, out.String(), "FINAL WARNING")
}

func TestConfirmEmptyTrashRejectsWrongSecondAnswer(t *testing.T) {
	command, out := newTrashConfirmationCommand("yes\nno\n")
	err := confirmEmptyTrash(command, trashTestItems())
	require.EqualError(t, err, "aborted by user")
	require.Contains(t, out.String(), "Type EMPTY TRASH to continue")
}

func TestReadConfirmationNormalizesInput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("  yes  \n"))
	got, err := readConfirmation(reader)
	require.NoError(t, err)
	require.Equal(t, "YES", got)
}

func TestTrashAPISetsAuthFile(t *testing.T) {
	// An empty auth path is rejected before any network call.
	_, err := trashAPI(context.Background(), "")
	require.Error(t, err)
}
