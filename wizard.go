package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// The interactive scaffold wizard — `bun create vite` for ultra products.
//
// Two laws hold this file down, and both are mechanically enforced by
// wizard_test.go rather than trusted:
//
//  1. The wizard owns QUESTIONS, never scaffolding. It collects answers,
//     renders them as an `ultra new` command line, and re-enters cmdNew with
//     it. There is exactly one scaffolding path in this binary and the wizard
//     is a caller of it, so a product born from the form and a product born
//     from the flags are the same product by construction.
//
//  2. Every wizard run PRINTS its equivalent command. A form is a fine way to
//     learn a tool and a terrible way to reproduce a result: the echoed line
//     is what goes in a README, a CI file, or an agent's prompt. It is shown
//     before the scaffold (so the summary can be checked) and again after (so
//     it can be copied out of the scrollback).
//
// The form never asks a question whose answer is forced, and can never produce
// a combination the flag parser rejects — see normalize.

// wizardWanted answers "did a human at a terminal type `ultra new` alone".
//
// Every clause is load-bearing. Any argument at all means the caller is using
// the flag form and must get it byte-identically, the missing-name help
// included. A piped stdin or stdout is an agent, a CI job, or a `| head` — a
// prompt there is a hang, not a question, so the wizard must be unreachable.
// And out is checked against the real os.Stdout because every test in this
// package passes a buffer: no test can reach a form that waits for a keystroke
// even if it has stubbed isTerminal for its own reasons.
func wizardWanted(args []string, out io.Writer) bool {
	return len(args) == 0 && out == os.Stdout &&
		isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

// The capability keys. They are the multi-select's values AND the flag names,
// which is why they are the same strings — the echoed command is built by
// prefixing them, so a rename cannot leave the form and the command disagreeing.
const (
	capDB   = "db"
	capWeb  = "web"
	capAuth = "auth"
	capMQTT = "mqtt"
)

// wizardAnswers is everything the form collects, and the wizard's ENTIRE
// output. Nothing else crosses out of this file: the answers become a command
// line, and the command line does the work.
type wizardAnswers struct {
	Name string
	DB   bool
	Web  bool
	Auth bool
	MQTT bool
	DS   string
}

// normalize applies the rules the flag parser would otherwise enforce by
// rejecting the command — so the wizard resolves them instead of failing on
// them. This is the ONE place the dependencies live, and both derivations
// below run through it, which is why the summary and the command can never
// describe different products.
func (a wizardAnswers) normalize() wizardAnswers {
	// The broker's CONNECT gate authenticates every device, so `--mqtt`
	// without `--auth` is a usage error. The form says so on the line and
	// turns auth on here; the user never meets the error.
	if a.MQTT {
		a.Auth = true
	}
	// A design system with no frontend to style is the other usage error, and
	// the DS question is hidden when web is off — so drop whatever a
	// previously-answered-then-revised run left behind.
	if !a.Web {
		a.DS = ""
	} else if a.DS == "" {
		a.DS = dsConnected
	}
	return a
}

// args renders the answers as `ultra new`'s arguments.
//
// Every default-on capability is stated explicitly — `--web` when it is on,
// `--no-web` when it is off — rather than relying on the parser's defaults.
// An echoed command is a promise about what it builds, and a command that
// only lists what to ADD would silently pick up whatever the defaults become
// in a later release.
func (a wizardAnswers) args() []string {
	a = a.normalize()
	args := []string{a.Name}
	for _, c := range []struct {
		flag      string
		on        bool
		byDefault bool
	}{
		{capDB, a.DB, true}, {capWeb, a.Web, true},
		{capAuth, a.Auth, true}, {capMQTT, a.MQTT, false},
	} {
		switch {
		case c.on:
			args = append(args, "--"+c.flag)
		case c.byDefault:
			args = append(args, "--no-"+c.flag)
		}
	}
	if a.Web {
		args = append(args, "--ds", a.DS)
	}
	return args
}

// command is args as the line a human pastes.
func (a wizardAnswers) command() string {
	return "ultra new " + strings.Join(a.args(), " ")
}

// data is the product the SUMMARY describes. It is built straight from the
// answers rather than by parsing args() — the two derivations are independent
// on purpose, because a summary computed from the command it prints could
// never disagree with it, and so could never catch the bug that matters: a
// form that shows one product and builds another.
// TestWizardEmitsTheSameScaffoldAsItsEchoedCommand pins them together.
func (a wizardAnswers) data() scaffoldData {
	a = a.normalize()
	return scaffoldData{
		Name:      a.Name,
		Module:    "example.com/" + a.Name,
		Version:   resolveVersion(""),
		GoVersion: scaffoldGoVersion,
		DB:        a.DB,
		Web:       a.Web,
		Auth:      a.Auth,
		MQTT:      a.MQTT,
		DS:        a.DS,
	}
}

// summary is the one-line shape, shown before anything is written. capsSuffix
// is the same renderer the "created" line uses, so the shape named here and
// the shape reported after the scaffold are one string function apart.
func (a wizardAnswers) summary() string {
	d := a.data()
	line := d.Name + " — " + strings.ReplaceAll(strings.TrimPrefix(capsSuffix(d), ", "), "+", ", ")
	if d.Web {
		line += " · " + d.DS + " DS"
	}
	return line + " · Go " + d.GoVersion + " · " + d.Version
}

// validateProductName is the name question's live check. It calls nameRe —
// the regexp the flag path validates against — rather than restating the rule,
// so the form can never accept a name `scaffold` would go on to reject.
func validateProductName(s string) error {
	if s == "" {
		return errors.New("a product needs a name")
	}
	if !nameRe.MatchString(s) {
		return fmt.Errorf("%q must be lowercase letters, digits, and dashes, starting with a letter", s)
	}
	return nil
}

// wizardForm is the questions plus the values huh writes into. Caps is the
// multi-select's own shape — the list of capability keys that are on — which
// answers folds back into the booleans everything downstream uses.
//
// The fields are EXPORTED because huh recomputes a TitleFunc only when the
// hash of its bindings changes, and hashstructure — the hasher huh uses —
// ignores unexported fields. Bound to a struct of unexported fields the
// summary would hash equal forever and render whatever it computed first,
// which is the empty answer set.
type wizardForm struct {
	Name string
	Caps []string
	DS   string
	OK   bool
}

// answers reads the collected values back out. Normalized here so a caller
// cannot get an un-resolved combination out of the form at all.
func (f *wizardForm) answers() wizardAnswers {
	return wizardAnswers{
		Name: strings.TrimSpace(f.Name),
		DB:   slices.Contains(f.Caps, capDB),
		Web:  slices.Contains(f.Caps, capWeb),
		Auth: slices.Contains(f.Caps, capAuth),
		MQTT: slices.Contains(f.Caps, capMQTT),
		DS:   f.DS,
	}.normalize()
}

// build assembles the questions: name, capabilities, design system, and the
// summary the last one confirms. Each group is one screen; esc walks back
// through them and ctrl-c leaves with ErrUserAborted before anything is written.
//
// in is a parameter rather than os.Stdin so a test can drive the form to a
// decision — including an immediate EOF — with no possibility of a `go test`
// run finding a real terminal and stopping on a prompt.
func (f *wizardForm) build(in io.Reader, out io.Writer) *huh.Form {
	f.Caps = []string{capDB, capWeb, capAuth} // the paved road, preselected
	f.DS = dsConnected

	name := huh.NewInput().
		Title("Product name").
		Description("lowercase, digits, dashes — becomes the module, binary, and log name").
		Placeholder("watchpost").
		Value(&f.Name).
		Validate(validateProductName)

	caps := huh.NewMultiSelect[string]().
		Title("Capabilities").
		// Static options with a LIVE description. Deriving the options from
		// the selection (huh's OptionsFunc) would let the auth box tick
		// itself, which is the prettier version of this, but the field
		// renders empty under it — the options reload asynchronously and the
		// list never arrives. The dependency is announced here instead, the
		// instant mqtt is ticked, and applied by normalize, which is the
		// thing that actually guarantees it.
		Options(
			huh.NewOption("db     postgres + migrations + sqlc stores", capDB),
			huh.NewOption("web    embedded SvelteKit SPA", capWeb),
			huh.NewOption("auth   identity + route enforcement — secure by default", capAuth),
			huh.NewOption("mqtt   embedded broker + Events surfaces — requires auth, toggles it on", capMQTT),
		).
		// The dependency resolves in front of the user, as they toggle. The
		// note on the mqtt line says what will happen; this says that it has.
		DescriptionFunc(func() string {
			if slices.Contains(f.Caps, capMQTT) {
				return "mqtt requires auth — toggles it on (the broker's CONNECT gate authenticates every device)"
			}
			return "space toggles · enter accepts"
		}, &f.Caps).
		Value(&f.Caps)

	// Asked ONLY when there is a frontend to style: `--ds` without `--web` is
	// a usage error, and a question whose answer cannot matter is noise.
	ds := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Design system").
			Description("the frontend's styling foundation").
			Options(
				huh.NewOption("connected   the company DS from depot", dsConnected),
				huh.NewOption("bare        Tailwind only — pick this without depot access", dsBare),
			).
			Value(&f.DS),
	).WithHideFunc(func() bool { return !slices.Contains(f.Caps, capWeb) })

	confirm := huh.NewGroup(
		huh.NewConfirm().
			TitleFunc(func() string { return f.answers().summary() }, f).
			DescriptionFunc(func() string {
				return "equivalent command (reproducible, CI-safe):\n  " + f.answers().command()
			}, f).
			Affirmative("Scaffold it").
			Negative("Cancel").
			Value(&f.OK),
	)

	form := huh.NewForm(huh.NewGroup(name), huh.NewGroup(caps), ds, confirm).
		WithTheme(wizardTheme(out)).
		WithKeyMap(wizardKeyMap()).
		WithInput(in).
		WithOutput(out).
		WithShowHelp(true)
	// A terminal that cannot draw is asked in plain text instead: huh's
	// accessible mode prints numbered prompts and reads lines. An ABSENT TERM
	// counts — dev.go makes the same reading, that a child with no TERM should
	// assume the dumbest terminal there is.
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		form = form.WithAccessible(true)
	}
	return form
}

// wizardKeyMap is huh's default with esc bound to "back" everywhere, which is
// the key a human reaches for and the one the design promises. ctrl-c stays
// the abort.
func wizardKeyMap() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	for _, b := range []*key.Binding{
		&k.Input.Prev, &k.Select.Prev, &k.MultiSelect.Prev, &k.Confirm.Prev,
	} {
		*b = key.NewBinding(key.WithKeys("shift+tab", "esc"), key.WithHelp("esc", "back"))
	}
	return k
}

// wizardTheme dresses huh in ultra's diagnostic voice: the ◆/◇ gutter, the
// same green-for-chosen the banners use, and colour decided by colorFor — the
// ONE rule in this binary, so NO_COLOR silences the form exactly as it
// silences everything else.
func wizardTheme(out io.Writer) *huh.Theme {
	t := huh.ThemeBase()
	gutter := lipgloss.Border{Left: "│"}
	t.Focused.Base = t.Focused.Base.Border(gutter, false, false, false, true).PaddingLeft(1)
	t.Blurred.Base = t.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
	// In a single-select the cursor IS the answer, so it carries the ◆. In a
	// multi-select the ◆/◇ is the checkbox and the cursor is a separate thing
	// that has to look different — the two glyphs land in the same row.
	t.Focused.SelectSelector = lipgloss.NewStyle().SetString("◆ ")
	t.Focused.MultiSelectSelector = lipgloss.NewStyle().SetString("❯ ")
	t.Focused.SelectedPrefix = lipgloss.NewStyle().SetString("◆ ")
	t.Focused.UnselectedPrefix = lipgloss.NewStyle().SetString("◇ ")
	t.Blurred.SelectSelector = lipgloss.NewStyle().SetString("  ")
	t.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString("  ")
	t.Blurred.SelectedPrefix = t.Focused.SelectedPrefix
	t.Blurred.UnselectedPrefix = t.Focused.UnselectedPrefix
	if !colorFor(out) {
		return t
	}
	// ANSI-16 only, like the rest of cmd/ultra's palette: it survives every
	// terminal and every theme a person might be running.
	green, yellow, dim := lipgloss.Color("2"), lipgloss.Color("3"), lipgloss.Color("8")
	t.Focused.Title = t.Focused.Title.Foreground(green).Bold(true)
	t.Focused.Description = t.Focused.Description.Foreground(dim)
	t.Focused.Base = t.Focused.Base.BorderForeground(green)
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(yellow)
	t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(yellow)
	t.Focused.SelectedPrefix = t.Focused.SelectedPrefix.Foreground(green)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(green)
	t.Focused.UnselectedPrefix = t.Focused.UnselectedPrefix.Foreground(dim)
	t.Focused.ErrorIndicator = t.Focused.ErrorIndicator.Foreground(lipgloss.Color("1"))
	t.Focused.ErrorMessage = t.Focused.ErrorMessage.Foreground(lipgloss.Color("1"))
	t.Group.Title = t.Focused.Title
	t.Group.Description = t.Focused.Description
	t.Blurred.Title = t.Blurred.Title.Foreground(dim)
	t.Blurred.Description = t.Blurred.Description.Foreground(dim)
	return t
}

// cmdNewWizard asks, then acts. The two halves are separate functions because
// only the first one needs a terminal.
func cmdNewWizard(in io.Reader, out, errW io.Writer) int {
	var f wizardForm
	return f.finish(f.build(in, out).Run(), out, errW)
}

// finish turns the form's outcome into an exit code, and is the ONLY place the
// wizard can begin a scaffold. Split from the form itself so the abort paths
// are provable in a test: driving a real TUI to a decision needs a terminal,
// and a test that cannot get one would hang rather than fail.
//
// Nothing has been written when this is called, so every path that returns
// before cmdNew leaves the working directory untouched.
func (f *wizardForm) finish(err error, out, errW io.Writer) int {
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Fprintln(errW, "aborted — nothing written")
			return 1
		}
		fmt.Fprintln(errW, err)
		failVerdict(errW, "new", err.Error())
		return 1
	}
	if !f.OK {
		fmt.Fprintln(errW, "cancelled — nothing written")
		return 1
	}
	a := f.answers()
	// The wizard's whole output is this command line. From here on the run is
	// indistinguishable from a human having typed it.
	if code := cmdNew(a.args(), out, errW); code != 0 {
		return code
	}
	fmt.Fprintf(out, "\nThis run, as a command:\n  %s\n", a.command())
	// The two doors the wizard deliberately does not ask about. --from is an
	// on-ramp for a service that already exists, and compliance is a typed
	// opt-in of its own — neither belongs in the path of a first product, but
	// both should be findable by someone who just made one.
	fmt.Fprint(out, `
Growing this product:
  ultra new <name> --from <openapi.json>   # scaffold from a legacy service's document
  ultra compliance init                    # opt in to the ISO 29110 records tree
`)
	return 0
}
