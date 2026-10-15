package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	fabrikplugin "github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

// The workflow migration (#2139, ADR 2139). `fabrik upgrade` rewrites the stage
// files' skill references to fully-qualified form and, when the deployed
// plugin in .fabrik/plugin carries operator edits, moves that tree into
// .fabrik/plugins/local-workflows (manifest name "local-workflows") and
// restores a clean base plugin. It decides only from what is on disk and the
// per-file table of shipped versions, never from version numbers, so a
// deployment that skipped releases reaches the same end state, and a second run
// changes nothing. The daemon never runs it; startup only reports it pending.

const (
	pluginManifestRel = ".claude-plugin/plugin.json"
	stagesRelDir      = ".fabrik/stages"
)

// stageFileEdit is one stage file whose skill references will be rewritten.
type stageFileEdit struct {
	path    string
	rel     string
	content []byte
	changes []stages.SkillRefChange
}

// migrationPlan is what planMigration found; building it writes nothing.
type migrationPlan struct {
	dir string

	// Skip, when set, is why the migration does not apply at all (a vendored
	// development plugin set): report only.
	Skip string
	// Blocked, when set, is why a needed move was refused: local-workflows
	// already exists and is never overwritten.
	Blocked string

	// MoveTree moves .fabrik/plugin to .fabrik/plugins/local-workflows.
	MoveTree bool
	// NormaliseLocal makes the local-workflows manifest name correct and drops
	// the .installed-version file that described the base plugin, not it.
	NormaliseLocal bool
	// RestoreBase rewrites .fabrik/plugin from the binary's embedded plugin.
	RestoreBase bool

	// Edited are the skills whose files differ from every shipped version.
	Edited []string
	// StageEdits are the stage files to rewrite.
	StageEdits []stageFileEdit
	// Notes are informational lines for the report.
	Notes []string
}

// Pending reports whether running the migration would change anything.
func (p *migrationPlan) Pending() bool {
	if p.Skip != "" || p.Blocked != "" {
		return false
	}
	return p.MoveTree || p.NormaliseLocal || p.RestoreBase || len(p.StageEdits) > 0
}

func isDirPath(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// planMigration inspects the deployment under dir. devOverride is the
// development plugin override (FABRIK_PLUGIN_DIR); when set, or when
// .fabrik/plugin is a symlink, the plugin set is vendored and the migration is
// reported rather than applied.
func planMigration(dir string, isDev bool, devOverride string) (*migrationPlan, error) {
	p := &migrationPlan{dir: dir}
	pluginDir := filepath.Join(dir, ".fabrik", "plugin")
	lwDir := filepath.Join(dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))

	if devOverride != "" {
		p.Skip = fmt.Sprintf("a development plugin override is set (%s); the plugin set is vendored, so nothing was moved or rewritten. Run the migration without the override, or move your customisations to %s by hand", devOverride, fabrikplugin.LocalWorkflowsRelDir)
		return p, nil
	}
	if fi, err := os.Lstat(pluginDir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		p.Skip = ".fabrik/plugin is a symlink (a vendored plugin set), so nothing was moved or rewritten; move your customisations to " + fabrikplugin.LocalWorkflowsRelDir + " by hand"
		return p, nil
	}

	lwExists := isDirPath(lwDir)
	baseExists := isDirPath(pluginDir)

	var baseCls fabrikplugin.Classification
	custom := false
	if baseExists {
		var err error
		custom, _, _, err = fabrikplugin.CheckPluginState(pluginDir, isDev)
		if err != nil {
			return nil, fmt.Errorf("checking plugin state: %w", err)
		}
		if baseCls, err = fabrikplugin.ClassifyDir(pluginDir); err != nil {
			return nil, fmt.Errorf("classifying %s: %w", pluginDir, err)
		}
	}

	// cls describes the tree the edited skills are read from.
	cls := baseCls
	switch {
	case custom && baseCls.Customised():
		if lwExists {
			p.Blocked = fmt.Sprintf("%s already exists and is never overwritten; .fabrik/plugin has local edits that could not be moved. Move or rename one of them, then run 'fabrik upgrade' again", fabrikplugin.LocalWorkflowsRelDir)
			return p, nil
		}
		if err := checkManifestRenamable(pluginDir, fabrikplugin.LocalWorkflowsName); err != nil {
			p.Blocked = fmt.Sprintf("the customised plugin's manifest cannot be renamed safely, so nothing was moved (%v). Make \"name\" a plain top-level string in %s, then run 'fabrik upgrade' again", err, pluginManifestRel)
			return p, nil
		}
		p.MoveTree, p.NormaliseLocal, p.RestoreBase = true, true, true
	case custom:
		// A mixture of shipped versions, no edit: not a customisation. Refresh.
		p.RestoreBase = true
	}
	// usableLocal is whether the local-workflows plugin on disk is (or will be,
	// once this run normalises it) named local-workflows, so a stage reference
	// qualified with that name will resolve.
	usableLocal := p.MoveTree
	// ownLocal is whether local-workflows is the operator's own plugin rather
	// than a moved copy of the base plugin (#2203).
	ownLocal := false
	if lwExists && !p.MoveTree {
		// local-workflows from an earlier (possibly interrupted) run, or the
		// operator's own: the edited skills are read from it.
		lwCls, err := fabrikplugin.ClassifyDir(lwDir)
		if err != nil {
			return nil, fmt.Errorf("classifying %s: %w", lwDir, err)
		}
		cls = lwCls
		// Only a directory that is recognisably a moved copy of the base plugin
		// is touched: its manifest still names the base plugin (the move
		// happened, the rename did not) or it has no manifest at all. A
		// directory whose manifest names anything else is the operator's own
		// plugin; renaming it would break every reference to that name, so it
		// is left exactly as it is.
		name, nameErr := readManifestName(lwDir)
		switch {
		case nameErr == nil && name == fabrikplugin.LocalWorkflowsName:
			usableLocal = true
		case errors.Is(nameErr, fs.ErrNotExist) || (nameErr == nil && isBasePluginName(name)):
			if err := checkManifestRenamable(lwDir, fabrikplugin.LocalWorkflowsName); err != nil {
				p.Blocked = fmt.Sprintf("the manifest in %s cannot be renamed safely, so nothing was changed (%v). Make \"name\" a plain top-level string in %s, then run 'fabrik upgrade' again", fabrikplugin.LocalWorkflowsRelDir, err, pluginManifestRel)
				return p, nil
			}
			p.NormaliseLocal = true
			usableLocal = true
		case nameErr != nil:
			p.Notes = append(p.Notes, fmt.Sprintf("%s has an unreadable manifest (%v); it was left alone and no stage was pointed at it", fabrikplugin.LocalWorkflowsRelDir, nameErr))
		default:
			ownLocal = true
			p.Notes = append(p.Notes, fmt.Sprintf("%s is a plugin named %q, not %q; it was left alone and no stage was pointed at it", fabrikplugin.LocalWorkflowsRelDir, name, fabrikplugin.LocalWorkflowsName))
		}
		if len(baseCls.Files) == 0 {
			p.RestoreBase = true
		}
	}

	hasLocal := usableLocal
	if hasLocal {
		p.Edited = cls.EditedSkills
	}
	shipped := map[string]bool{}
	for _, s := range fabrikplugin.ShippedSkillNames() {
		shipped[s] = true
	}
	edited := map[string]bool{}
	for _, s := range p.Edited {
		edited[s] = true
	}

	// The plugins beyond the base the daemon would load: a bare reference one of
	// them provides is qualified to it, as the runtime resolver would pick it,
	// rather than to the base plugin (#2203).
	project, projectNotes := loadProjectPlugins(dir, ownLocal)
	p.Notes = append(p.Notes, projectNotes...)

	stagesDir := filepath.Join(dir, filepath.FromSlash(stagesRelDir))
	entries, err := os.ReadDir(stagesDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", stagesDir, err)
	}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		path := filepath.Join(stagesDir, e.Name())
		rel := stagesRelDir + "/" + e.Name()
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		out, changes, err := stages.RewriteSkillRefs(src, func(key, ref string) (string, bool) {
			// A pre-#2140 reference to a base skill (the old qualified form or
			// the bare fabrik-<stage> name; ADR 2140). The same table the runtime
			// resolver falls back on decides what it maps to. If the operator
			// edited that skill, the edit lives in local-workflows, so the
			// reference points there and the edit survives. edited is keyed by
			// the skill name on disk: the old name (fabrik-<stage>) for a tree
			// edited before the rename, the new name (<stage>) for a tree the
			// binary had already refreshed to the renamed plugin before the
			// operator edited it — the legacy mapping resolves this reference to
			// that edited copy at runtime, so both are checked.
			if newRef, legacy := fabrikplugin.LegacyRef(ref); legacy {
				_, oldSkill, _ := fabrikplugin.Split(ref)
				if hasLocal {
					if edited[oldSkill] {
						return fabrikplugin.Qualify(fabrikplugin.LocalWorkflowsName, oldSkill), true
					}
					if _, newSkill, _ := fabrikplugin.Split(newRef); edited[newSkill] {
						return fabrikplugin.Qualify(fabrikplugin.LocalWorkflowsName, newSkill), true
					}
				}
				// A bare reference a project plugin also provides keeps resolving
				// there, as the runtime resolver (as-is lookup first) does; the
				// qualified pre-rename form is explicit and is not second-guessed.
				if _, _, isQualified := fabrikplugin.Split(ref); !isQualified {
					if projRef, note, ok := project.qualify(rel, key, ref); ok {
						p.Notes = append(p.Notes, note)
						return projRef, true
					}
				}
				return newRef, true
			}
			pn, skill, qualified := fabrikplugin.Split(ref)
			if qualified {
				if pn == fabrikplugin.BasePluginName {
					// edited is keyed by the skill name on disk: the new name for a
					// customised copy of the renamed plugin, the pre-rename name
					// (fabrik-<stage>) for a tree edited before the rename. A
					// reference to the base plugin carries the new name, so check both.
					editedAs := ""
					if edited[skill] {
						editedAs = skill
					} else if old := "fabrik-" + skill; edited[old] {
						if _, ok := fabrikplugin.LegacySkillTarget(old); ok {
							editedAs = old
						}
					}
					if editedAs != "" {
						p.Notes = append(p.Notes, fmt.Sprintf("%s: %s is %q, already qualified and left alone — your edited copy of that skill is %s", rel, key, ref, fabrikplugin.Qualify(fabrikplugin.LocalWorkflowsName, editedAs)))
					}
				}
				return "", false
			}
			switch {
			case hasLocal && edited[ref]:
				return fabrikplugin.Qualify(fabrikplugin.LocalWorkflowsName, ref), true
			case shipped[ref]:
				return fabrikplugin.Qualify(fabrikplugin.BasePluginName, ref), true
			}
			if projRef, note, ok := project.qualify(rel, key, ref); ok {
				p.Notes = append(p.Notes, note)
				return projRef, true
			}
			p.Notes = append(p.Notes, fmt.Sprintf("%s: %s %q names no shipped skill and was left bare; qualify it with its plugin name by hand", rel, key, ref))
			return "", false
		})
		if err != nil {
			p.Notes = append(p.Notes, fmt.Sprintf("%s: skipped (%v)", rel, err))
			continue
		}
		if len(changes) > 0 {
			p.StageEdits = append(p.StageEdits, stageFileEdit{path: path, rel: rel, content: out, changes: changes})
		}
	}
	sort.Slice(p.StageEdits, func(i, j int) bool { return p.StageEdits[i].rel < p.StageEdits[j].rel })
	return p, nil
}

// isBasePluginName reports whether name is the base plugin's current name or
// the one it carried before #2140. A copy of the base plugin that an earlier
// `fabrik upgrade` moved into local-workflows still declares the old name.
func isBasePluginName(name string) bool {
	return name == fabrikplugin.BasePluginName || name == fabrikplugin.LegacyBasePluginName
}

func readManifestName(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(pluginManifestRel)))
	if err != nil {
		return "", err
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}
	return m.Name, nil
}

// topLevelNameSpan returns the byte span of the top-level "name" string value
// in a JSON object, ignoring any "name" nested in another object (the shipped
// manifest has author.name). With duplicate top-level keys it returns the last,
// which is the one json.Unmarshal keeps.
func topLevelNameSpan(data []byte) (start, end int, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil {
		return 0, 0, err
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, errors.New("manifest is not a JSON object")
	}
	start, end = -1, -1
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		key, _ := keyTok.(string)
		keyEnd := int(dec.InputOffset())
		if key != "name" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, err
			}
			continue
		}
		valTok, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		if _, ok := valTok.(string); !ok {
			return 0, 0, errors.New(`the top-level "name" is not a string`)
		}
		q := bytes.IndexByte(data[keyEnd:], '"')
		if q < 0 {
			return 0, 0, errors.New(`cannot locate the top-level "name" value`)
		}
		start, end = keyEnd+q, int(dec.InputOffset())
	}
	if start < 0 {
		return 0, 0, errors.New(`no top-level "name" field`)
	}
	return start, end, nil
}

// renamedManifest returns data with only its top-level name value set to name,
// verified by decoding both versions. changed is false when it already is.
func renamedManifest(data []byte, name string) (out []byte, changed bool, err error) {
	var orig map[string]any
	if err := json.Unmarshal(data, &orig); err != nil {
		return nil, false, err
	}
	if cur, _ := orig["name"].(string); cur == name {
		return data, false, nil
	}
	start, end, err := topLevelNameSpan(data)
	if err != nil {
		return nil, false, err
	}
	quoted, err := json.Marshal(name)
	if err != nil {
		return nil, false, err
	}
	out = append(append(append([]byte{}, data[:start]...), quoted...), data[end:]...)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		return nil, false, fmt.Errorf("renamed manifest does not parse: %w", err)
	}
	orig["name"] = name
	if !reflect.DeepEqual(orig, got) {
		return nil, false, errors.New("renaming would change more than the name")
	}
	return out, true, nil
}

// checkManifestRenamable reports whether renameManifest would succeed on the
// manifest in dir, writing nothing. Planning calls it before any tree is moved,
// so a manifest that cannot be renamed refuses the migration up front instead
// of failing after .fabrik/plugin has already been renamed aside.
func checkManifestRenamable(dir, name string) error {
	path := filepath.Join(dir, filepath.FromSlash(pluginManifestRel))
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, _, err := renamedManifest(data, name); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// renameManifest makes the manifest in dir declare name, changing only that
// value. A missing manifest is created minimally so the plugin can load.
// It reports whether the file was written.
func renameManifest(dir, name string) (bool, error) {
	path := filepath.Join(dir, filepath.FromSlash(pluginManifestRel))
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		body := fmt.Sprintf("{\n  \"name\": %q,\n  \"description\": \"Local workflow customisations moved out of .fabrik/plugin by fabrik upgrade\",\n  \"version\": \"1.0.0\"\n}\n", name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return false, err
		}
		return true, os.WriteFile(path, []byte(body), 0644)
	}
	if err != nil {
		return false, err
	}
	out, changed, err := renamedManifest(data, name)
	if err != nil {
		return false, fmt.Errorf("renaming the manifest in %s: %w", path, err)
	}
	if !changed {
		return false, nil
	}
	return true, writeFileAtomic(path, out)
}

// writeFileAtomic replaces path with data via a temp file and rename, keeping
// the existing file's permissions.
func writeFileAtomic(path string, data []byte) error {
	mode := fs.FileMode(0644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fabrik-upgrade-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// applyMigration performs the plan and writes a report to out. The order makes
// every intermediate state detectable and re-runnable: the customised tree is
// renamed aside first (the destination is never overwritten), then made a
// loadable plugin, then the base is restored, then the stage files are
// rewritten. A crash after any step is picked up by the next run's plan from
// what is on disk.
func applyMigration(p *migrationPlan, out io.Writer) error {
	pluginDir := filepath.Join(p.dir, ".fabrik", "plugin")
	lwDir := filepath.Join(p.dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir))
	var moved []string

	if p.MoveTree {
		if _, err := os.Lstat(lwDir); err == nil {
			return fmt.Errorf("%s appeared since planning; not overwriting it", fabrikplugin.LocalWorkflowsRelDir)
		}
		if err := os.MkdirAll(filepath.Dir(lwDir), 0755); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Dir(lwDir), err)
		}
		if err := os.Rename(pluginDir, lwDir); err != nil {
			return fmt.Errorf("moving .fabrik/plugin to %s: %w", fabrikplugin.LocalWorkflowsRelDir, err)
		}
		moved = append(moved, fmt.Sprintf("moved .fabrik/plugin -> %s", fabrikplugin.LocalWorkflowsRelDir))
	}
	if p.NormaliseLocal {
		if err := os.Remove(filepath.Join(lwDir, ".installed-version")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing .installed-version from %s: %w", fabrikplugin.LocalWorkflowsRelDir, err)
		}
		changed, err := renameManifest(lwDir, fabrikplugin.LocalWorkflowsName)
		if err != nil {
			return fmt.Errorf("renaming the plugin in %s: %w", fabrikplugin.LocalWorkflowsRelDir, err)
		}
		if changed {
			moved = append(moved, fmt.Sprintf("renamed the plugin in %s/%s to %q", fabrikplugin.LocalWorkflowsRelDir, pluginManifestRel, fabrikplugin.LocalWorkflowsName))
		}
	}
	if p.RestoreBase {
		wrote, err := fabrikplugin.RefreshPluginTo(pluginDir)
		if err != nil {
			return fmt.Errorf("restoring .fabrik/plugin: %w", err)
		}
		if err := fabrikplugin.WriteInstalledVersion(pluginDir); err != nil {
			return fmt.Errorf("writing installed version: %w", err)
		}
		moved = append(moved, fmt.Sprintf("restored .fabrik/plugin from the binary (%d file(s))", wrote))
	}
	for _, e := range p.StageEdits {
		if err := writeFileAtomic(e.path, e.content); err != nil {
			return fmt.Errorf("rewriting %s: %w", e.rel, err)
		}
	}
	printMigrationReport(p, moved, out)
	return nil
}

func printMigrationReport(p *migrationPlan, moved []string, out io.Writer) {
	fmt.Fprintln(out, "fabrik upgrade: workflow migration")
	for _, m := range moved {
		fmt.Fprintf(out, "  %s\n", m)
	}
	if len(p.Edited) > 0 && (p.MoveTree || isDirPath(filepath.Join(p.dir, filepath.FromSlash(fabrikplugin.LocalWorkflowsRelDir)))) {
		fmt.Fprintf(out, "  edited skills kept in %s: %s\n", fabrikplugin.LocalWorkflowsRelDir, strings.Join(p.Edited, ", "))
	}
	for _, e := range p.StageEdits {
		for _, c := range e.changes {
			fmt.Fprintf(out, "  %s: %s %s -> %s\n", e.rel, c.Key, c.Old, c.New)
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
	fmt.Fprintln(out, "  Review the result and commit it yourself (fabrik never commits to your repository):")
	fmt.Fprintf(out, "    git add .fabrik/plugin %s .fabrik/stages\n", fabrikplugin.LocalWorkflowsRelDir)
	fmt.Fprintf(out, "  To undo: restore those paths with git, or move %s back to .fabrik/plugin and set its manifest name back to the base plugin's (%q, or %q before the stage plugin was renamed).\n", fabrikplugin.LocalWorkflowsRelDir, fabrikplugin.BasePluginName, fabrikplugin.LegacyBasePluginName)
}

// printMigrationNotice reports a skipped or blocked migration.
func printMigrationNotice(p *migrationPlan, out io.Writer) {
	switch {
	case p.Skip != "":
		fmt.Fprintf(out, "fabrik upgrade: workflow migration skipped: %s\n", p.Skip)
	case p.Blocked != "":
		fmt.Fprintf(out, "fabrik upgrade: workflow migration refused: %s\n", p.Blocked)
	}
}

// migrationPendingDetail describes what `fabrik upgrade` would do, for the
// warnings panel. Unresolved bare names are not part of it: they cannot be
// migrated, so they must not keep the entry raised.
func migrationPendingDetail(p *migrationPlan) string {
	var parts []string
	if p.MoveTree {
		parts = append(parts, fmt.Sprintf(".fabrik/plugin has local edits that will move to %s", fabrikplugin.LocalWorkflowsRelDir))
	}
	if p.RestoreBase && !p.MoveTree {
		parts = append(parts, ".fabrik/plugin will be restored from the binary")
	}
	if p.NormaliseLocal && !p.MoveTree {
		parts = append(parts, fmt.Sprintf("%s needs finishing (an interrupted migration)", fabrikplugin.LocalWorkflowsRelDir))
	}
	if n := len(p.StageEdits); n > 0 {
		parts = append(parts, fmt.Sprintf("%d stage file(s) have skill references to qualify", n))
	}
	return "A workflow migration is pending: " + strings.Join(parts, "; ") + "."
}
