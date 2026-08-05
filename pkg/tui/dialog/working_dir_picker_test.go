package dialog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkingDirPickerRootHasNoParentDirEntry(t *testing.T) {
	t.Parallel()

	// Get the root of the current working directory
	cwd, err := os.Getwd()
	require.NoError(t, err)
	root := filepath.VolumeName(cwd) + string(filepath.Separator)

	d := NewWorkingDirPickerDialog(t.Context(), nil, nil, nil, root).(*workingDirPickerDialog)

	// Ensure there's no ".." entry in the browse entries
	for _, e := range d.browseEntries {
		if e.name == ".." {
			t.Errorf("root directory should not have a parent dir entry, but got '..'")
		}
	}
}

func TestWorkingDirPickerEmptyInitialDirUsesGetwd(t *testing.T) {
	t.Parallel()

	// Pass an empty string for the initial directory.
	// NewWorkingDirPickerDialog should fall back to os.Getwd().
	d := NewWorkingDirPickerDialog(t.Context(), nil, nil, nil, "").(*workingDirPickerDialog)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	require.Equal(t, cwd, d.currentDir, "empty initial directory should fall back to current working directory")
}
