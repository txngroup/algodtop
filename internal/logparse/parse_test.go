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
		// Relays by DNS name, one template for all of them.
		"network performance monitor - peer 'r-ab.relays.example.net:4160' delay 81 first message portion 34%": "network performance monitor - peer '<addr>' delay <n> first message portion <n>%",
		"TxSyncer.Sync: asking client r-cd.relays.example.net:4160 for missing transactions":                   "TxSyncer.Sync: asking client <addr> for missing transactions",
		"Peer relay-7.example.org:4160 disconnected: LeastPerformingPeer":                                      "Peer <addr> disconnected: LeastPerformingPeer",
		// A source position is not a host.
		"unexpected state at ledger.go:412": "unexpected state at ledger.go:412",
		// libp2p DHT refresh keys are mostly padding and can lack a digit.
		"starting refreshing cpl 3 with key CIQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAPQ (routing table size was 12)": "starting refreshing cpl <n> with key <id> (routing table size was <n>)",
		// Upper-case words stay.
		"BROADCAST_QUEUE_FULL dropped TX": "BROADCAST_QUEUE_FULL dropped TX",
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

// algod names a closed account that still has a key installed each time
// agreement asks for voting keys, several times a round.
func TestClosedAccount(t *testing.T) {
	const addr = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAY5HFKQ"
	l := Parse([]byte(`{"file":"node.go","function":"github.com/algorand/go-algorand/node.(*AlgorandFullNode).VotingKeys","level":"info","msg":"node.VotingKeys: Address: ` + addr + ` - Account was closed but still has a participation key active.","time":"2026-11-03T14:33:12.027Z"}`))
	if l.Extract == nil || l.Extract.Type != "closed-account" || l.Extract.Addr != addr {
		t.Fatalf("extract %+v", l.Extract)
	}
	if l.Template != "node.VotingKeys: Address: <account> - Account was closed but still has a participation key active." {
		t.Errorf("template %q", l.Template)
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

func TestTemplateSpans(t *testing.T) {
	cases := map[string]string{
		// IPv6 peers, bracketed with a port and bare.
		"peer[[2001:db8:1::5]:4160] line=12 read err: read: connection reset by peer":                               "peer[<addr>] line=<n> read err: read: connection reset by peer",
		"failed to write CloseMessage to connection for [2001:db8::9]:4160, err: write: broken pipe":                "failed to write CloseMessage to connection for <addr>, err: write: broken pipe",
		"wsPeer readLoop: received a TS response for a stale request from [2001:db8:ab::]:4160. 12 bytes discarded": "wsPeer readLoop: received a TS response for a stale request from <addr>. <n> bytes discarded",
		"listening on [::]:4160":             "listening on <addr>",
		"dial from 2001:db8:5f:: failed":     "dial from <addr> failed",
		"route via 2001:db8::abcd: no route": "route via <addr>: no route",
		"[::ffff:192.0.2.1]:4160 refused":    "<addr> refused",
		// Brackets around something else stay.
		"peer[203.0.113.9:4160] read err: EOF": "peer[<addr>] read err: EOF",
		"slice [1:2] out of range":             "slice [1:2] out of range",
		"state a::b changed":                   "state a::b changed",
		"at 12:34:56 the clock moved":          "at <n>:<n>:<n> the clock moved",
		// Dates with weekday and month names.
		`Headers = http.Header{"Date":[]string{"Sat, 21 Nov 2026 23:36:25 GMT"}}, Body = <html>`: `Headers = http.Header{"Date":[]string{"<date>"}}, Body = <html>`,
		"cert expires Mon, 02 Jan 2006 15:04:05 -0700 soon":                                      "cert expires <date> soon",
		"since Tue, 05 May 2026 08:00:00 CEST":                                                   "since <date>",
		"started Wed Jan  7 15:04:05 UTC 2026":                                                   "started <date>",
		"started Thu Dec 31 23:59:59 2026, ok":                                                   "started <date>, ok",
		// A relay behind TLS.
		"ws connect(wss://relay.example.net:443/v1/mainnet-v1.0/gossip) fail": "ws connect(<url>) fail",
		// A weekday word without a date stays.
		"Mon, the first day": "Mon, the first day",
	}
	for in, want := range cases {
		if got := Template(in); got != want {
			t.Errorf("Template(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// A relay's failed handshake on two days is one template.
	const line = `ws connect(wss://relay.example.net:443/v1/mainnet-v1.0/gossip) fail - bad handshake, Status code = 404, Headers = http.Header{"Content-Type":[]string{"text/html"}, "Date":[]string{"%s"}}`
	a := Template(strings.Replace(line, "%s", "Sat, 21 Nov 2026 23:36:25 GMT", 1))
	b := Template(strings.Replace(line, "%s", "Mon, 01 Dec 2026 01:02:03 GMT", 1))
	if a != b {
		t.Errorf("two days, two templates:\n%q\n%q", a, b)
	}
}
