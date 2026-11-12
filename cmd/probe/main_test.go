package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseProcNetTCP(t *testing.T) {
	tmp := t.TempDir()
	tcpFile := filepath.Join(tmp, "tcp")
	// Port 8000 in hex is 1F40:
	// - 1 LISTEN socket (0A) with backlog_max=0x80 (128), accept_queue=0x03 (3)
	// - 2 ESTAB sockets (01)
	// - 1 TIME_WAIT socket (06)
	// - 1 CLOSE_WAIT socket (08)
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F40 00000000:0000 0A 00000080:00000003 00:00000000 00000000     0        0 10001 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F40 0100007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 10002 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:1F40 0100007F:C351 01 00000000:00000000 00:00000000 00000000     0        0 10003 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:1F40 0100007F:C352 06 00000000:00000000 03:0000012C 00000000     0        0 0 3 0000000000000000
   4: 0100007F:1F40 0100007F:C353 08 00000000:00000000 00:00000000 00000000     0        0 10004 1 0000000000000000 20 4 30 10 -1
`
	if err := os.WriteFile(tcpFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	states, listen := parseProcNetTCP(tcpFile, 8000)
	if states["ESTAB"] != 2 {
		t.Errorf("ESTAB = %d; want 2", states["ESTAB"])
	}
	if states["TIME_WAIT"] != 1 {
		t.Errorf("TIME_WAIT = %d; want 1", states["TIME_WAIT"])
	}
	if states["CLOSE_WAIT"] != 1 {
		t.Errorf("CLOSE_WAIT = %d; want 1", states["CLOSE_WAIT"])
	}
	if listen["accept_queue"] != 3 {
		t.Errorf("accept_queue = %d; want 3", listen["accept_queue"])
	}
	if listen["backlog_max"] != 128 {
		t.Errorf("backlog_max = %d; want 128", listen["backlog_max"])
	}
}
