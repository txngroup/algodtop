// Command fakealgod runs an algod stand-in for the Docker remote tests and
// for trying algodtop without a real node.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"algod.top/internal/fakealgod"
)

func main() {
	dir := flag.String("d", "", "data directory to write algod.net, algod.pid, tokens and node.log into")
	addr := flag.String("l", "127.0.0.1:0", "listen address")
	network := flag.String("network", "mainnet", "network name")
	usage := flag.Bool("peer-usage", false, "report per-peer usage fields")
	token := flag.String("token", "", "API token")
	admin := flag.String("admin-token", "", "admin API token")
	noauth := flag.Bool("no-auth", false, "DisableAPIAuth")
	round := flag.Duration("round", 2800*time.Millisecond, "round interval")
	churn := flag.Duration("churn", 20*time.Second, "peer churn interval")
	trace := flag.Int("trace", 20, "trace lines per round")
	peers := flag.Int("peers", 20, "peers")
	first := flag.Uint64("first-round", 0, "first round, default 52184330")
	genesis := flag.String("genesis-hash", "", "genesis hash, base64")
	ver := flag.String("version", "4.4.1", "algod version, major.minor.build")
	branch := flag.String("branch", "", "build branch, for example rel/stable")
	channel := flag.String("channel", "", "build channel, for example stable or beta")
	docIPs := flag.Bool("doc-ips", false, "peer addresses from the documentation ranges")
	load := flag.Float64("load", 1, "scale how fast counters grow, such as traffic")
	jitter := flag.Bool("jitter", false, "vary round times like a real network")
	uptime := flag.Duration("uptime", 24*time.Hour, "how long the process seems to have run at start, which sets its counters")
	wave := flag.Bool("wave", false, "let traffic and other rates rise and fall over minutes and hours")
	wire := flag.Bool("wire", false, "move the reported traffic over loopback through a helper process, so wire measurements see it")
	sink := flag.Bool("wire-sink", false, "run as the -wire helper: print a listen address and serve until stdin closes")
	var accounts []fakealgod.Account
	flag.Func("account", "an online account with a participation key on this node, as SEED:ALGOS:KEYDAYS (repeatable)", func(s string) error {
		p := strings.Split(s, ":")
		if len(p) != 3 {
			return fmt.Errorf("want SEED:ALGOS:KEYDAYS")
		}
		algos, err := strconv.ParseUint(p[1], 10, 64)
		if err != nil {
			return err
		}
		days, err := strconv.ParseFloat(p[2], 64)
		if err != nil {
			return err
		}
		accounts = append(accounts, fakealgod.Account{Address: fakealgod.Address(p[0]), Amount: algos * 1e6, KeyDays: days})
		return nil
	})
	flag.Parse()
	if *sink {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(ln.Addr())
		go fakealgod.ServeWireSink(ln)
		io.Copy(io.Discard, os.Stdin)
		return
	}
	// The sink is a child process with this binary. It exits when its
	// stdin closes, which happens when this process ends however it ends.
	var wireTo string
	var sinkIn io.WriteCloser
	if *wire {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cmd := exec.Command(exe, "-wire-sink")
		cmd.Stderr = os.Stderr
		sinkIn, _ = cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "wire sink:", err)
			os.Exit(1)
		}
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil {
			fmt.Fprintln(os.Stderr, "wire sink:", err)
			os.Exit(1)
		}
		wireTo = strings.TrimSpace(line)
	}
	var v [3]int
	if _, err := fmt.Sscanf(*ver, "%d.%d.%d", &v[0], &v[1], &v[2]); err != nil {
		fmt.Fprintln(os.Stderr, "-version:", err)
		os.Exit(2)
	}
	f, err := fakealgod.Start(fakealgod.Options{DataDir: *dir, Addr: *addr, Network: *network, PeerUsage: *usage,
		Token: *token, AdminToken: *admin, DisableAuth: *noauth, RoundEvery: *round, ChurnEvery: *churn, LogTraceRate: *trace,
		Peers: *peers, Round: *first, GenesisHash: *genesis, Version: v, Branch: *branch, Channel: *channel, DocIPs: *docIPs, Accounts: accounts, Load: *load, Jitter: *jitter, Uptime: *uptime,
		Wave: *wave, WireTo: wireTo})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("fakealgod listening on", f.Addr())
	for _, a := range accounts {
		fmt.Println("account", a.Address)
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	f.Close()
	if sinkIn != nil {
		sinkIn.Close()
	}
}
