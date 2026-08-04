package dialog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/skills"
)

func TestNewSkillsDialog_EmptyShowsPlaceholder(t *testing.T) {
	t.Parallel()
	d := NewSkillsDialog(nil).(*skillsDialog)
	out := strings.Join(d.renderLines(80, 24), "\n")
	assert.Contains(t, out, "Skills (0)")
	assert.Contains(t, out, "No skills available")
}

func TestNewSkillsDialog_RendersSkills(t *testing.T) {
	t.Parallel()
	skillList := []skills.Skill{
		{
			Name:        "commit",
			Description: "Commit local changes",
			BaseDir:     "skills/commit",
			Local:       true,
			Context:     "fork",
		},
		{
			Name:        "poem",
			Description: "Prints a poem",
			BaseDir:     "cache/skills/poem",
			Local:       false,
		},
	}
	d := NewSkillsDialog(skillList).(*skillsDialog)
	out := strings.Join(d.renderLines(80, 24), "\n")
	assert.Contains(t, out, "Skills (2)")
	assert.Contains(t, out, "commit")
	assert.Contains(t, out, "Commit local changes")
	assert.Contains(t, out, filepath.FromSlash("from: skills/commit"))
	assert.Contains(t, out, "local")
	assert.Contains(t, out, "fork")
	assert.Contains(t, out, "poem")
	assert.Contains(t, out, "Prints a poem")
	assert.Contains(t, out, filepath.FromSlash("from: cache/skills/poem"))
	assert.Contains(t, out, "remote")
}

func TestSkillLoadedFrom_LocalHomeSkillUsesTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	skill := &skills.Skill{
		BaseDir: filepath.Join(home, ".agents", "skills", "commit"),
		Local:   true,
	}

	assert.Equal(t, filepath.Join("~", ".agents", "skills", "commit"), skillLoadedFrom(skill))
}

func TestSkillLoadedFrom_LocalProjectSkillUsesRelativePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	repo := t.TempDir()
	cwd := filepath.Join(repo, "subdir")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	t.Chdir(cwd)

	skill := &skills.Skill{
		BaseDir: filepath.Join(repo, ".agents", "skills", "commit"),
		Local:   true,
	}

	assert.Equal(t, filepath.Join("..", ".agents", "skills", "commit"), skillLoadedFrom(skill))
}
