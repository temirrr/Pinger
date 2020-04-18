package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	protoNum = 1 // ipv4.ICMPTypeEchoReply.Protocol()
)

func parseArgs(hostPtr *string, ipVersionPtr *string, ttlPtr *int) {
	ipVersionFlag := flag.String("ip", "ipv4", "IP version: {ipv4|ipv6}.")
	ttlFlag := flag.Int("ttl", 100, "Specifies TTL (Time to live).")
	Usage := func() {
		fmt.Fprintf(os.Stderr, "Usage : %s:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	*ipVersionPtr = *ipVersionFlag
	*ttlPtr = *ttlFlag
	*hostPtr = flag.Arg(0)
	if flag.NArg() == 0 {
		Usage()
		os.Exit(1)
	}

	fmt.Printf(
		"Hostname or IP address: %s, IP version: %s, ttl: %d.\n",
		*hostPtr,
		*ipVersionPtr,
		*ttlPtr,
	)
}

func timeToBytes(t time.Time) []byte {
	bytes := make([]byte, 8)
	nsecs := t.UnixNano()
	for i := 0; i < 8; i++ {
		bytes[i] = byte(0xff & (nsecs >> ((7 - i) * 8)))
	}

	return bytes
}

func bytesToTime(bytes []byte) time.Time {
	nsecs := int64(0)
	for i := 0; i < 8; i++ {
		nsecs += int64(bytes[i]) << ((7 - i) * 8)
	}

	return time.Unix(nsecs/1000000000, nsecs%1000000000)
}

func getConnection(network, address string) *icmp.PacketConn {
	conn, err := icmp.ListenPacket(network, address)
	if err != nil {
		fmt.Printf("Opening connection error: %s.\n", err)
		os.Exit(1)
	}
	return conn
}

// PingProc is a client's ping process.
type PingProc struct {
	id       int
	seqnum   int
	dst      net.IPAddr
	rttLimit time.Duration
	interval time.Duration // time between echo signals
}

func newPingProc(dstIP net.IPAddr) *PingProc {
	// ensuring new seed value everytime
	rand.Seed(time.Now().UnixNano())
	return &PingProc{
		id:     rand.Intn(1 << 16), // mock random id
		seqnum: rand.Intn(1 << 16),
		// (TODO): add zone for IPv6
		dst:      dstIP,
		rttLimit: 2000 * time.Millisecond,
		interval: 1300 * time.Millisecond,
	}
}

func (p *PingProc) sendEcho(cn *icmp.PacketConn) error {
	msgType := ipv4.ICMPTypeEcho
	t := timeToBytes(time.Now())

	// checksum is calculated by `Marshal` method
	bytes, _ := (&icmp.Message{
		Type: msgType,
		Code: 0,
		Body: &icmp.Echo{
			ID:   p.id,
			Seq:  p.seqnum,
			Data: t,
		},
	}).Marshal(nil)

	if _, err := cn.WriteTo(bytes, &p.dst); err != nil {
		sendErr := fmt.Errorf("Send echo error: %s", err)
		return sendErr
	}

	return nil
}

type recvResult struct {
	bytes []byte
	err   error
}

func (p *PingProc) recvEchoReply(cn *icmp.PacketConn, ch chan recvResult) {
	for {
		bytes := make([]byte, 512)

		_, peer, err := cn.ReadFrom(bytes)
		if err != nil {
			recvErr := fmt.Errorf("Send echo error: %s", err)
			ch <- recvResult{nil, recvErr}
			return
		}

		var msg *icmp.Message
		if msg, err = icmp.ParseMessage(protoNum, bytes); err != nil {
			recvErr := fmt.Errorf("Send echo error: %s", err)
			ch <- recvResult{nil, recvErr}
			return
		}

		switch msg.Type {
		case ipv4.ICMPTypeEchoReply:
			log.Printf("got reflection from %v", peer)
			ch <- recvResult{bytes, nil}
		default:
			log.Printf("got %+v; want echo reply", msg)
			recvErr := fmt.Errorf("Send echo error: not echo reply message type")
			ch <- recvResult{nil, recvErr}
		}
	}
}

func (p *PingProc) handleRecv(bytes []byte) {
	var msg *icmp.Message
	var err error
	if msg, err = icmp.ParseMessage(protoNum, bytes); err != nil {
		fmt.Printf("Handle receive error: %s", err)
		return
	}

	var rtt time.Duration
	switch body := msg.Body.(type) {
	case *icmp.Echo:
		if body.ID == p.id && body.Seq == p.seqnum {
			rtt = time.Since(bytesToTime(body.Data))
		}
	default:
		return // silently return
	}

	fmt.Printf("64 bytes from %s: time=%v\n", p.dst.IP.String(), rtt)
}

func pingLoop(p *PingProc, cn *icmp.PacketConn) error {
	ping := make(chan recvResult)
	go p.recvEchoReply(cn, ping)
	p.sendEcho(cn)
	timer := time.NewTimer(p.rttLimit)

	for {
		select {
		case <-timer.C:
			p.seqnum++
			fmt.Printf("unreachable: %s.\n", p.dst.IP.String())
		case res := <-ping:
			p.handleRecv(res.bytes)
			timer.Stop()
			time.Sleep(p.interval)
		}

		timer.Reset(p.rttLimit)
		if err := p.sendEcho(cn); err != nil {
			break
		}
	}

	timer.Stop()
	return nil
}

func main() {
	var host, ipVersion string
	var ttl int
	network := "ip4:icmp"

	parseArgs(&host, &ipVersion, &ttl)

	res, err := net.ResolveIPAddr(network, host)
	if err != nil {
		fmt.Printf("Address resolving error: %s.\n", err)
		os.Exit(1)
	}

	p := newPingProc(net.IPAddr{IP: res.IP, Zone: res.Zone})
	cn := getConnection(network, "")

	if err := pingLoop(p, cn); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
