package logparse

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) []Line {
	t.Helper()
	f, err := os.Open("testdata/mainnet.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, Parse(sc.Bytes()))
	}
	return out
}

func TestFixtureLines(t *testing.T) {
	lines := fixture(t)
	types := map[string]int{}
	kinds := map[Kind]int{}
	for _, l := range lines {
		kinds[l.Kind]++
		if l.Extract != nil {
			types[l.Extract.Type]++
		}
		if l.Kind != KindBad && l.Time.IsZero() {
			t.Errorf("no time on %+v", l)
		}
	}
	if kinds[KindTrace] != 2 || kinds[KindFlood] != 2 {
		t.Errorf("kinds %v", kinds)
	}
	for _, want := range []string{"connect", "disconnect", "handshake-eof", "gossip-stream", "outgoing", "incoming", "conn-reset"} {
		if types[want] == 0 {
			t.Errorf("no %s extract, got %v", want, types)
		}
	}
	for _, l := range lines {
		if l.Extract != nil && l.Extract.Type == "disconnect" {
			if l.Extract.Reason == "" || !strings.HasPrefix(l.Extract.Addr, "/ip4/") {
				t.Errorf("disconnect %+v", l.Extract)
			}
		}
		if l.Kind == KindFlood && l.AppID != "2468013579" {
			t.Errorf("flood app %q", l.AppID)
		}
		if l.Kind == KindTrace && l.TraceType == "" {
			t.Error("trace type missing")
		}
	}
}

func TestTemplates(t *testing.T) {
	cases := map[string]string{
		"committed round 66602021 with pre-validated block {{} 0 RQZECB37WTAXEJFTNX5YK5EE3ZZINR775WT6JLUXJICSSRYSW3OLU7VRFM PZJHGLSK7JAL2UAMRNVCVX3XX6SFACBWXRQ6J6S4OBASOLCOU6NA}": "committed round <n> with pre-validated block {{} <n> <account> <id>}",
		"wsStreamHandlerV22: error reading peer meta headers response from peer 12D3KooWBuS5F7v2sBgtLKWCshaSUEJzTR4NLK4Kzhfn2mEkmRrW (/ip4/203.0.113.5/tcp/48696): EOF":            "wsStreamHandlerV22: error reading peer meta headers response from peer <peer> (<maddr>): EOF",
		"peer[203.0.113.9:4160] read err: timeout after 1.5s": "peer[<addr>] read err: timeout after <dur>",
		"finished round 66602021":                             "finished round <n>",
	}
	for in, want := range cases {
		if got := Template(in); got != want {
			t.Errorf("Template(%q)\n got  %q\n want %q", in, got, want)
		}
	}
	a := Template("attested to {{} 0 HC2OMUXEJWT7ENYNTYTA4JYTMVIKJI5G2B7VYDBTF6FREJAIH7JKKWRNIY}")
	b := Template("attested to {{} 0 FOIC7CIR5AMBR6GJTVOV3GBRSV2QJWIOSRO6F2HVJ3TYDTDV6Y3KXT7PVU}")
	if a != b {
		t.Errorf("attested templates differ: %q vs %q", a, b)
	}
}

func BenchmarkPrefilterTrace(b *testing.B) {
	line := []byte(`{"Context":"Agreement","Hash":"PZJHGLSK7JAL2UAMRNVCVX3XX6SFACBWXRQ6J6S4OBASOLCOU6NA","ObjectPeriod":0,"ObjectRound":66602021,"ObjectStep":2,"Period":0,"Round":66602021,"Sender":"BPZWAOWGNYUGGZYDVMA45CB4U2BS3BG45AG2HCRXVMDZNDV7WTIHWHWXYM","Step":2,"Type":"VoteAccepted","Weight":12,"WeightTotal":423,"file":"trace.go","function":"x","level":"info","line":1,"msg":"vote","time":"2026-11-03T12:37:00.706691Z"}`)
	for b.Loop() {
		Parse(line)
	}
}

// An outgoing ws peer logs its IP as Address and the host it dialled as
// Endpoint; the session keys on the endpoint, as /v2/node/peers does.
func TestWSDisconnectUsesEndpoint(t *testing.T) {
	l := Parse([]byte(`{"details":{"Address":"192.0.2.7","Incoming":false,"Endpoint":"relay.example.net:4160","Reason":"LeastPerformingPeer"},"file":"telemetry.go","level":"info","msg":"/Network/DisconnectPeer","time":"2026-11-03T14:33:12.027Z"}`))
	if l.Extract == nil || l.Extract.Addr != "relay.example.net:4160" || l.Extract.IP != "192.0.2.7" {
		t.Fatalf("extract %+v", l.Extract)
	}
}

func TestFastTime(t *testing.T) {
	for _, s := range []string{"2026-11-03T12:37:00.706691Z", "2026-11-03T12:37:00Z", "2026-11-03T12:37:00.7Z", "2026-11-03T23:59:59.123456789Z"} {
		want, _ := time.Parse(time.RFC3339Nano, s)
		got, ok := fastTime([]byte(s))
		if !ok || !got.Equal(want) {
			t.Errorf("%s: got %v %v want %v", s, got, ok, want)
		}
	}
	if _, ok := fastTime([]byte("2026-11-03 12:37:00")); ok {
		t.Error("accepted a space separator")
	}
}

func TestAccessLine(t *testing.T) {
	const self = `{"file":"logger.go","function":"github.com/algorand/go-algorand/daemon/algod/api/server/lib/middlewares.(*LoggerMiddleware).handler.func1","level":"info","line":56,"msg":"127.0.0.1:53709 - - [2026-11-03 17:56:51.718537 +0000 GMT m=+88600.377185959] \"GET /v2/transactions/pending?max=1 HTTP/1.1\" 200 866 \"algodtop\" 149.709` + "\u00b5" + `s","time":"2026-11-03T17:56:51.718709Z"}`
	l := Parse([]byte(self))
	if l.Kind != KindHTTP || !l.Self || l.HTTPPath != "/v2/transactions/pending" || l.HTTPCode != "200" {
		t.Fatalf("%+v", l)
	}
	const other = `{"file":"logger.go","level":"info","msg":"10.0.0.5:4242 - - [2026-11-03 17:56:51 +0000 GMT] \"GET /v2/accounts/ABC HTTP/1.1\" 404 12 \"curl/8.7.1\" 1ms","time":"2026-11-03T17:56:51Z"}`
	l = Parse([]byte(other))
	if l.Kind != KindHTTP || l.Self || l.HTTPCode != "404" {
		t.Fatalf("%+v", l)
	}
	// Another logger.go message stays a template.
	if l := Parse([]byte(`{"file":"logger.go","level":"info","msg":"something else","time":"2026-11-03T17:56:51Z"}`)); l.Kind != KindTemplate {
		t.Fatalf("%+v", l)
	}
}
