package apply

import (
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/config"
)

func TestReportSaysSoUnmistakablyWhenThereIsNothingToDo(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	delete(runner.fail, "nft list table inet regied")

	report := reportOf(t, engine, cfg)

	if !strings.Contains(report, "Nothing to do") {
		t.Errorf("a plan that changes nothing does not say so:\n%s", report)
	}
}

func TestReportPutsTheWarningsBeforeAnythingElse(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture))
	plan.Warnings = append(plan.Warnings, "a declaration that could not be rendered as written")
	plan.Notes = append(plan.Notes, "the line is not up")

	report := renderReport(plan)

	warning := strings.Index(report, "could not be rendered as written")
	first := strings.Index(report, "/etc/systemd/network")
	if warning < 0 {
		t.Fatalf("the warning is missing:\n%s", report)
	}
	if first >= 0 && warning > first {
		t.Errorf("a warning is printed after the diffs:\n%s", report)
	}
	if !strings.Contains(report, "the line is not up") {
		t.Errorf("a note about what the host could not answer is missing:\n%s", report)
	}
}

func TestReportNeverPrintsACredential(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture))

	report := renderReport(plan)

	for _, secret := range []string{"hunter2", "account@example.net"} {
		if strings.Contains(report, secret) {
			t.Errorf("the report carries the credential %q:\n%s", secret, report)
		}
	}
	// It has to say the file would be written, with what mode, and whether its content
	// changed: that is what decides whether the session will be restarted (ADR 0006).
	if !strings.Contains(report, "/etc/regied/ppp/credentials/pppoe0.conf") {
		t.Errorf("the credentials file is not reported at all:\n%s", report)
	}
	if !strings.Contains(report, "0600") {
		t.Errorf("the credentials file's mode is not reported:\n%s", report)
	}
	if !strings.Contains(report, "content not shown") {
		t.Errorf("the report does not say why the content is missing:\n%s", report)
	}
	// The peer file is the half that is safe to print, and a dry-run is useless without
	// it.
	if !strings.Contains(report, "plugin rp-pppoe.so") {
		t.Errorf("the peer file is not shown, so the useful half of the dry-run is blank:\n%s", report)
	}
}

func TestReportShowsTheDiffOfAFileThatChanged(t *testing.T) {
	engine, _, runner, _ := planFixture(t)
	mustApply(t, engine, load(t, hostFixture))
	delete(runner.fail, "nft list table inet regied")

	changed := load(t, strings.Replace(hostFixture, "192.168.10.127", "192.168.10.200", 1))
	report := reportOf(t, engine, changed)

	if !strings.Contains(report, "-dhcp-range") && !strings.Contains(report, "+dhcp-range") {
		t.Errorf("the change to the address handout is not shown as a diff:\n%s", report)
	}
	if !strings.Contains(report, "unchanged") {
		t.Errorf("the files that would not change are not accounted for:\n%s", report)
	}
}

func TestReportListsTheCommandsInTheOrderTheyWouldRun(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture))

	report := renderReport(plan)

	nft := strings.Index(report, "nft -f -")
	networkctl := strings.Index(report, "networkctl reload")
	session := strings.Index(report, "regied-pppoe@pppoe0.service")
	if nft < 0 || networkctl < 0 || session < 0 {
		t.Fatalf("not every command is reported:\n%s", report)
	}
	if !(nft < networkctl && networkctl < session) {
		t.Errorf("the commands are not in the order they would run:\n%s", report)
	}
	if !strings.Contains(report, "net.ipv4.ip_forward") {
		t.Errorf("the kernel switches that would change are not reported:\n%s", report)
	}
}

func TestReportShowsTheRulesetInFullWhenThereIsNoneToCompareWith(t *testing.T) {
	engine, _, _, _ := planFixture(t)
	plan := mustPlan(t, engine, load(t, hostFixture))

	report := renderReport(plan)

	if !strings.Contains(report, "table inet regied") {
		t.Errorf("the ruleset is not in the report:\n%s", report)
	}
}

// C-5. A file whose content is the same and whose mode is not still has to say what
// would change, or the line is a claim with nothing behind it.
func TestReportSaysWhatChangedWhenOnlyTheModeDid(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	delete(runner.fail, "nft list table inet regied")

	peer := "/etc/regied/ppp/peers/pppoe0.conf"
	content, _ := files.content(peer)
	files.put(peer, content, 0o600)

	report := reportOf(t, engine, cfg)

	if !strings.Contains(report, "mode 0600 -> 0644") {
		t.Errorf("a mode-only change does not say what would change:\n%s", report)
	}
	if strings.Contains(report, "@@") {
		t.Errorf("a file whose content did not change is shown as a diff:\n%s", report)
	}
}

// C-2. The warnings matter more on a real apply than on a dry run, not less, so they
// have to be printable on their own.
func TestWarningsCanBeReportedWithoutTheWholePlan(t *testing.T) {
	plan := &Plan{
		Warnings: []string{"a declaration that could not be rendered as written"},
		Notes:    []string{"the line is not up"},
	}

	var b strings.Builder
	ReportWarnings(&b, plan)

	for _, want := range []string{"could not be rendered as written", "the line is not up"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("%q is missing:\n%s", want, b.String())
		}
	}
	// Nothing else: this goes in front of an apply, not instead of one.
	if strings.Contains(b.String(), "Nothing to do") {
		t.Errorf("the warnings carry the closing line of a whole report:\n%s", b.String())
	}
}

// 差し戻し 2. A trailing newline is a difference a line diff cannot show, and compare
// calls it a change: the file is rewritten and whatever reads it is restarted. Saying
// "the content would not change" hides the one thing a dry run is for.
func TestReportSaysAFileIsRewrittenWhenTheDiffShowsNothing(t *testing.T) {
	engine, files, runner, _ := planFixture(t)
	cfg := load(t, hostFixture)
	mustApply(t, engine, cfg)
	delete(runner.fail, "nft list table inet regied")

	conf := "/etc/regied/dnsmasq/dnsmasq.conf"
	content, _ := files.content(conf)
	files.put(conf, strings.TrimSuffix(content, "\n"), 0o644)

	report := reportOf(t, engine, cfg)

	if strings.Contains(report, "the content would not change") {
		t.Errorf("a file that would be rewritten is reported as unchanged:\n%s", report)
	}
	if !strings.Contains(report, "rewritten") {
		t.Errorf("nothing says the file would be rewritten:\n%s", report)
	}
}

func reportOf(t *testing.T, engine *Engine, cfg *config.Config) string {
	t.Helper()
	return renderReport(mustPlan(t, engine, cfg))
}

func renderReport(plan *Plan) string {
	var b strings.Builder
	Report(&b, plan)
	return b.String()
}
