package hcl

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestToYAML_FileFunction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	instructionsPath := filepath.Join(dir, "instructions.txt")
	require.NoError(t, os.WriteFile(instructionsPath, []byte("Line 1\nLine 2\n"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("instructions.txt")
  model       = "auto"
}
`)

	m, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.NoError(t, err)

	items := m["agents"].(yaml.MapSlice)
	root := items[0].Value.(map[string]any)
	assert.Equal(t, "Line 1\nLine 2\n", root["instruction"])
}

func TestToYAML_FileFunctionTemplate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("You are ${name}, level ${level}.\n"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("prompt.md", { name = "Gopher", level = 3 })
  model       = "auto"
}
`)

	m, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.NoError(t, err)

	items := m["agents"].(yaml.MapSlice)
	root := items[0].Value.(map[string]any)
	assert.Equal(t, "You are Gopher, level 3.\n", root["instruction"])
}

func TestToYAML_FileFunctionTemplateHasNoFunctions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("${file(\"prompt.md\")}"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("prompt.md", {})
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rendering template")
}

func TestToYAML_FileFunctionTemplateDirectives(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tmpl := "Rules:\n%{ for rule in rules ~}\n- ${rule}\n%{ endfor ~}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rules.md"), []byte(tmpl), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("rules.md", { rules = ["be brief", "be kind"] })
  model       = "auto"
}
`)

	m, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.NoError(t, err)

	items := m["agents"].(yaml.MapSlice)
	root := items[0].Value.(map[string]any)
	assert.Equal(t, "Rules:\n- be brief\n- be kind\n", root["instruction"])
}

func TestToYAML_FileFunctionTemplateMissingVariable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("Hello ${name} ${missing}"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("prompt.md", { name = "Gopher" })
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rendering template")
	assert.Contains(t, err.Error(), "missing")
}

func TestRenderTemplateRejectsUnknownVars(t *testing.T) {
	t.Parallel()

	_, err := renderTemplate([]byte("Hello"), "prompt.md", cty.UnknownVal(cty.Map(cty.String)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "template variables must be an object")
}

func TestToYAML_FileFunctionTemplateVarsMustBeObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("Hello"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("prompt.md", "nope")
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "template variables must be an object")
}

func TestToYAML_FileFunctionWithoutVarsKeepsInterpolationLiteral(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("Run ${shell({cmd: \"ls\"})}\n"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("prompt.md")
  model       = "auto"
}
`)

	m, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.NoError(t, err)

	items := m["agents"].(yaml.MapSlice)
	root := items[0].Value.(map[string]any)
	assert.Equal(t, "Run ${shell({cmd: \"ls\"})}\n", root["instruction"])
}

func TestToYAML_FileFunctionMissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := []byte(`
agent "root" {
  instruction = file("missing.txt")
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading file")
	assert.Contains(t, err.Error(), "missing.txt")
}

func TestToYAML_FileFunctionRejectsTraversal(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "config")
	require.NoError(t, os.Mkdir(dir, 0o755))
	secret := filepath.Join(parent, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("nope"), 0o644))

	src := []byte(`
agent "root" {
  instruction = file("../secret.txt")
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading file")
	assert.Contains(t, err.Error(), "../secret.txt")
	assert.Contains(t, err.Error(), "local relative path")
}

func TestToYAML_FileFunctionRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "config")
	require.NoError(t, os.Mkdir(dir, 0o755))

	outside := filepath.Join(parent, "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))

	link := filepath.Join(dir, "instructions.txt")
	if err := os.Symlink("../outside.txt", link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	src := []byte(`
agent "root" {
  instruction = file("instructions.txt")
  model       = "auto"
}
`)

	_, err := ToMap(src, filepath.Join(dir, "agent.hcl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading file")
	assert.Contains(t, err.Error(), fmt.Sprintf("%q", filepath.Join(dir, "instructions.txt")))
}
