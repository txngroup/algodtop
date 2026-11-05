package cli

import (
	"strings"
	"testing"
)

func TestWindowsPlan(t *testing.T) {
	p := windowsPlan(`D:\Data\`, true)
	if p.name != "algodtop-agent" || p.display != "algod.top agent" || p.dir != `D:\Data\algodtop-agent` ||
		p.config != `D:\Data\algodtop-agent\agent.json` || p.socket != `D:\Data\algodtop-agent\agent.sock` ||
		p.store != `D:\Data\algodtop-agent\store` || p.bin != `D:\Data\algodtop-agent\bin\algod.top.exe` || p.log != `D:\Data\algodtop-agent\log\algod.top.log` {
		t.Fatalf("%+v", p)
	}
	if got := strings.Join(p.args, " "); got != `agent --config D:\Data\algodtop-agent\agent.json --store-dir D:\Data\algodtop-agent\store --socket D:\Data\algodtop-agent\agent.sock` {
		t.Error(got)
	}
	d := windowsPlan("", false)
	if d.name != "algodtop" || d.config != `C:\ProgramData\algodtop\config.json` || d.socket != `C:\ProgramData\algodtop\algodtop.sock` || d.args[0] != "daemon" {
		t.Errorf("%+v", d)
	}
	if winDir(p.bin) != `D:\Data\algodtop-agent\bin` {
		t.Error(winDir(p.bin))
	}
}
