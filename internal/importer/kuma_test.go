package importer

import (
	"strings"
	"testing"
	"time"

	"github.com/didvc/uptime-mon/internal/endpoints"
	"github.com/didvc/uptime-mon/internal/model"
)

const backup = `{
  "version": "1.23.13",
  "notificationList": [],
  "monitorList": [
    {"id":1,"name":"ping site","type":"ping","url":"https://","hostname":"example.com",
     "method":"GET","active":true,"interval":60,"timeout":48,"maxretries":0,
     "retryInterval":60,"maxredirects":10,"accepted_statuscodes":["200-299"],
     "keyword":"","invertKeyword":false,"ignoreTls":false,"upsideDown":false,
     "headers":null,"body":null,"parent":null,"tags":[]},

    {"id":2,"name":"https://a.example/","type":"http","url":"https://a.example/",
     "method":"GET","active":true,"interval":60,"timeout":48,"maxretries":0,
     "retryInterval":60,"maxredirects":10,"accepted_statuscodes":["200-299"],
     "invertKeyword":false,"ignoreTls":false,"upsideDown":false,
     "headers":"{\n  \"Cookie\": \"session=abc123\"\n}","body":null,
     "parent":null,"tags":[{"name":"app","value":""}]},

    {"id":3,"name":"kw","type":"keyword","url":"https://b.example/",
     "method":"GET","active":false,"interval":20,"timeout":16,"maxretries":2,
     "retryInterval":30,"maxredirects":0,"accepted_statuscodes":["200-299","404"],
     "keyword":"say \"hello\" ok","invertKeyword":true,"ignoreTls":true,
     "upsideDown":true,"headers":null,"body":null,"parent":null,"tags":[]},

    {"id":4,"name":"db","type":"port","url":"https://","hostname":"db.internal",
     "port":5432,"method":"GET","active":true,"interval":60,"timeout":0.8,
     "maxretries":0,"retryInterval":60,"accepted_statuscodes":["200-299"],
     "parent":null,"tags":[]},

    {"id":5,"name":"a dns monitor","type":"dns","url":"https://","hostname":"x",
     "method":"GET","active":true,"interval":60,"timeout":48,
     "accepted_statuscodes":["200-299"],"parent":null,"tags":[]},

    {"id":6,"name":"basic","type":"http","url":"https://c.example/","method":"GET",
     "active":true,"interval":60,"timeout":48,"maxretries":0,"retryInterval":60,
     "maxredirects":10,"accepted_statuscodes":["200-299"],
     "basic_auth_user":"user","basic_auth_pass":"pass",
     "parent":null,"tags":[]}
  ]
}`

func load(t *testing.T, opts Options) ([]model.Target, []Warning) {
	t.Helper()
	got, warns, err := Load(strings.NewReader(backup), opts)
	if err != nil {
		t.Fatal(err)
	}
	return got, warns
}

func TestImportSkipsInactiveAndUnsupported(t *testing.T) {
	got, warns := load(t, Options{})

	// Active + supported: ping, a.example, db, basic. The keyword monitor is
	// inactive and the dns monitor is unsupported.
	if len(got) != 4 {
		names := make([]string, len(got))
		for i, g := range got {
			names[i] = g.Name
		}
		t.Fatalf("got %d targets: %v", len(got), names)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Reason, `"dns"`) {
		t.Fatalf("warnings = %+v", warns)
	}
}

func TestImportIncludeInactive(t *testing.T) {
	got, _ := load(t, Options{IncludeInactive: true})
	if len(got) != 5 {
		t.Fatalf("got %d targets", len(got))
	}
	var kw *model.Target
	for i := range got {
		if got[i].Name == "kw" {
			kw = &got[i]
		}
	}
	if kw == nil {
		t.Fatal("keyword monitor missing")
	}
	if kw.Enabled {
		t.Error("inactive monitor should be disabled")
	}
	if kw.Kind != model.KindKeyword || kw.Keyword != `say "hello" ok` || !kw.InvertKeyword {
		t.Errorf("keyword monitor = %+v", *kw)
	}
	if !kw.Insecure || !kw.UpsideDown || kw.MaxRedirects != 0 {
		t.Errorf("flags lost: %+v", *kw)
	}
	if kw.Accept.String() != "200-299,404" {
		t.Errorf("accept = %s", kw.Accept)
	}
	if kw.Retries != 2 || kw.RetryInterval != 30*time.Second {
		t.Errorf("retries = %d / %v", kw.Retries, kw.RetryInterval)
	}
	// interval=20s, so the 16s Kuma timeout stays under it.
	if kw.Interval != 20*time.Second || kw.Timeout != 16*time.Second {
		t.Errorf("timing = %v / %v", kw.Interval, kw.Timeout)
	}
}

func TestTimeoutClampedBelowInterval(t *testing.T) {
	got, _ := load(t, Options{})
	for _, g := range got {
		if g.Interval > 0 && g.Timeout >= g.Interval {
			t.Errorf("%s: timeout %v >= interval %v", g.Name, g.Timeout, g.Interval)
		}
	}
	// Kuma's 48s-against-60s default already fits inside the interval, so it
	// is preserved rather than second-guessed.
	for _, g := range got {
		if g.Name == "ping site" && g.Timeout != 48*time.Second {
			t.Errorf("ping timeout = %v, want 48s unchanged", g.Timeout)
		}
	}

	// A timeout that would overlap the next probe is pulled back to 90% of
	// the interval.
	const overlapping = `{"monitorList":[{"id":1,"name":"slow","type":"http",
	  "url":"https://x.example/","method":"GET","active":true,
	  "interval":20,"timeout":48,"accepted_statuscodes":["200-299"],
	  "parent":null,"tags":[]}]}`
	over, _, err := Load(strings.NewReader(overlapping), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if over[0].Timeout != 18*time.Second {
		t.Errorf("clamped timeout = %v, want 18s", over[0].Timeout)
	}
}

func TestHeadersGroupsAndAuth(t *testing.T) {
	got, _ := load(t, Options{})
	byName := map[string]model.Target{}
	for _, g := range got {
		byName[g.Name] = g
	}

	a := byName["https://a.example/"]
	if len(a.Headers) != 1 || a.Headers[0].Key != "Cookie" || a.Headers[0].Value != "session=abc123" {
		t.Errorf("headers = %+v", a.Headers)
	}
	if a.Group != "app" {
		t.Errorf("group from tag = %q", a.Group)
	}
	if a.Host != "a.example" {
		t.Errorf("host = %q", a.Host)
	}

	// Basic auth becomes a plain Authorization header.
	b := byName["basic"]
	if len(b.Headers) != 1 || b.Headers[0].Key != "Authorization" ||
		b.Headers[0].Value != "Basic dXNlcjpwYXNz" {
		t.Errorf("basic auth header = %+v", b.Headers)
	}

	p := byName["ping site"]
	if p.Kind != model.KindPing || p.Host != "example.com" {
		t.Errorf("ping = %+v", p)
	}
	d := byName["db"]
	if d.Kind != model.KindTCP || d.Port != 5432 || d.Host != "db.internal" {
		t.Errorf("tcp = %+v", d)
	}
	// timeout 0.8s survives as a sub-second duration.
	if d.Timeout != 800*time.Millisecond {
		t.Errorf("fractional timeout = %v", d.Timeout)
	}
}

func TestGroupFromOptions(t *testing.T) {
	got, _ := load(t, Options{GroupFrom: "none", DefaultGroup: "imported"})
	for _, g := range got {
		if g.Group != "imported" {
			t.Errorf("%s group = %q", g.Name, g.Group)
		}
	}
}

func TestOnlyTypes(t *testing.T) {
	got, _ := load(t, Options{OnlyTypes: []string{"ping"}})
	if len(got) != 1 || got[0].Kind != model.KindPing {
		t.Fatalf("got %+v", got)
	}
}

// The point of the importer is to produce a file the parser accepts, so the
// two are tested together.
func TestImportedFileParsesBack(t *testing.T) {
	got, _ := load(t, Options{IncludeInactive: true})

	var buf strings.Builder
	if err := endpoints.Render(&buf, got, "imported from a test"); err != nil {
		t.Fatal(err)
	}
	back, err := endpoints.Parse(strings.NewReader(buf.String()), endpoints.BaseDefaults())
	if err != nil {
		t.Fatalf("rendered file does not parse: %v\n---\n%s", err, buf.String())
	}
	if len(back) != len(got) {
		t.Fatalf("%d targets in, %d out\n%s", len(got), len(back), buf.String())
	}

	byName := map[string]model.Target{}
	for _, b := range back {
		byName[b.Name] = b
	}
	for _, want := range got {
		g, ok := byName[want.Name]
		if !ok {
			t.Errorf("%q missing after round trip\n%s", want.Name, buf.String())
			continue
		}
		if g.Kind != want.Kind || g.URL != want.URL || g.Host != want.Host ||
			g.Port != want.Port || g.Keyword != want.Keyword ||
			g.InvertKeyword != want.InvertKeyword || g.Enabled != want.Enabled ||
			g.Insecure != want.Insecure || g.UpsideDown != want.UpsideDown ||
			g.Interval != want.Interval || g.Timeout != want.Timeout ||
			g.Retries != want.Retries || g.MaxRedirects != want.MaxRedirects ||
			g.Accept.String() != want.Accept.String() || g.Group != want.Group ||
			len(g.Headers) != len(want.Headers) {
			t.Errorf("round trip changed %q:\n got %+v\nwant %+v", want.Name, g, want)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	if _, _, err := Load(strings.NewReader(`{bad json`), Options{}); err == nil {
		t.Error("expected parse error")
	}
	if _, _, err := Load(strings.NewReader(`{"monitorList":[]}`), Options{}); err == nil {
		t.Error("expected empty-backup error")
	}
	// Only unsupported monitors is an error, not an empty success.
	only := `{"monitorList":[{"id":1,"name":"m","type":"mqtt","active":true,
	          "accepted_statuscodes":["200-299"]}]}`
	if _, warns, err := Load(strings.NewReader(only), Options{}); err == nil {
		t.Errorf("expected error, warns=%+v", warns)
	}
}
