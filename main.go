package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

func parseArgs(hostPtr *string, isIPv6Ptr *bool, ttlPtr *int) {
	flag.BoolVar(isIPv6Ptr, "6", false, "Set this flag if you want to use IPv6")
	flag.IntVar(ttlPtr, "t", 100, "Specifies TTL (Time to live).")
	flag.IntVar(ttlPtr, "ttl", 100, "Specifies TTL (Time to live).")
	Usage := func() {
		fmt.Fprintf(os.Stderr, "Usage : %s:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	*hostPtr = flag.Arg(0)
	if flag.NArg() == 0 {
		Usage()
		os.Exit(1)
	}
}

func printSetup(hostPtr *string, isIPv6Ptr *bool, ttlPtr *int) {
	ipVersionStr := "IPv4"
	if *isIPv6Ptr {
		ipVersionStr = "IPv6"
	}
	fmt.Printf(
		"PING %s, IP version: %s, ttl: %d.\n",
		*hostPtr,
		ipVersionStr,
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

// PingProc is a client's ping process.
type PingProc struct {
	id       int
	seqnum   int
	dst      net.IPAddr
	isIPv6   bool
	ttl      int
	rttLimit time.Duration
	interval time.Duration // time between echo signals
}

func newPingProc(dstIP net.IPAddr, isIPv6 bool, ttl int) *PingProc {
	// ensuring new seed value everytime
	rand.Seed(time.Now().UnixNano())

	return &PingProc{
		id:       rand.Intn(1 << 16),
		seqnum:   rand.Intn(1 << 16),
		dst:      dstIP,
		isIPv6:   isIPv6,
		ttl:      ttl,
		rttLimit: 2 * time.Second,
		interval: time.Second,
	}
}

func (p *PingProc) getConnection(network, address string) *icmp.PacketConn {
	conn, err := icmp.ListenPacket(network, address)
	if err != nil {
		fmt.Printf("Opening connection error: %s.\n", err)
		os.Exit(1)
	}

	if !p.isIPv6 {
		conn.IPv4PacketConn().SetControlMessage(ipv4.FlagTTL, true)
		conn.IPv4PacketConn().SetTTL(p.ttl)
	} else {
		conn.IPv6PacketConn().SetControlMessage(ipv6.FlagHopLimit, true)
		conn.IPv6PacketConn().SetHopLimit(p.ttl)
	}

	return conn
}

func (p *PingProc) sendEcho(cn *icmp.PacketConn) error {
	var msgType icmp.Type
	if !p.isIPv6 {
		msgType = ipv4.ICMPTypeEcho
	} else {
		msgType = ipv6.ICMPTypeEchoRequest
	}
	p.seqnum++
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
	msg *icmp.Message
	ttl int
	err error
}

func (p *PingProc) recvEchoReply(cn *icmp.PacketConn, ch chan recvResult) {
	for {
		bytes := make([]byte, 512)

		var ttl int
		var err error
		if !p.isIPv6 {
			var cm *ipv4.ControlMessage
			_, cm, _, err = cn.IPv4PacketConn().ReadFrom(bytes)
			if err != nil {
				recvErr := fmt.Errorf("Send echo error: %s", err)
				ch <- recvResult{nil, -1, recvErr}
				return
			}
			if cm != nil {
				ttl = cm.TTL
			}
		} else {
			var cm *ipv6.ControlMessage
			_, cm, _, err = cn.IPv6PacketConn().ReadFrom(bytes)
			if err != nil {
				recvErr := fmt.Errorf("Send echo error: %s", err)
				ch <- recvResult{nil, -1, recvErr}
				return
			}
			if cm != nil {
				ttl = cm.HopLimit
			}
		}

		var msg *icmp.Message
		protoNum := ipv4.ICMPTypeEchoReply.Protocol()
		if p.isIPv6 {
			protoNum = ipv6.ICMPTypeEchoReply.Protocol()
		}
		if msg, err = icmp.ParseMessage(protoNum, bytes); err != nil {
			recvErr := fmt.Errorf("Send echo error: %s", err)
			ch <- recvResult{nil, -1, recvErr}
			return
		}

		ch <- recvResult{msg, ttl, nil}
	}
}

func (p *PingProc) handleEchoReply(msg *icmp.Message, ttl int) {
	var rtt time.Duration
	switch body := msg.Body.(type) {
	case *icmp.Echo:
		if body.ID == p.id && body.Seq == p.seqnum {
			rtt = time.Since(bytesToTime(body.Data))
		}
	}

	fmt.Printf(
		"64 bytes from %s: icmp_seq=%d ttl=%d time=%dms\n",
		p.dst.IP.String(),
		p.seqnum,
		ttl, // incoming `ttl` is different from outgoing `p.ttl`
		rtt.Milliseconds(),
	)
}

func (p *PingProc) handleTimeExceeded() {
	fmt.Printf(
		"From %s: icmp_seq=%d Time exceeded: Hop limit\n",
		p.dst.IP.String(),
		p.seqnum,
	)
}

// handleMsg is a general received message handler.
func (p *PingProc) handleMsg(msg *icmp.Message, ttl int) {
	switch msg.Type {
	case ipv4.ICMPTypeEchoReply:
		fallthrough
	case ipv6.ICMPTypeEchoReply:
		p.handleEchoReply(msg, ttl)
	case ipv4.ICMPTypeTimeExceeded:
		fallthrough
	case ipv6.ICMPTypeTimeExceeded:
		p.handleTimeExceeded()
	default:
		fmt.Printf("Unexpected message type received.")
	}
}

func pingLoop(p *PingProc, cn *icmp.PacketConn) error {
	ping := make(chan recvResult)
	go p.recvEchoReply(cn, ping)
	p.sendEcho(cn)
	timer := time.NewTimer(p.rttLimit)

	for {
		select {
		case <-timer.C:
			fmt.Printf("unreachable: %s.\n", p.dst.IP.String())
		case res := <-ping:
			if res.err == nil {
				p.handleMsg(res.msg, res.ttl)
			} else {
				fmt.Printf("Error during message receiving: %s.\n", res.err)
			}
			timer.Stop()
			time.Sleep(p.interval)
		}

		timer.Reset(p.rttLimit)
		if err := p.sendEcho(cn); err != nil {
			fmt.Printf("Send error: %s.\n", err)
			break
		}
	}

	timer.Stop()
	return nil
}

func main() {
	var host string
	var isIPv6 bool
	var ttl int

	parseArgs(&host, &isIPv6, &ttl)

	if strings.Index(host, ":") != -1 {
		isIPv6 = true
	}

	printSetup(&host, &isIPv6, &ttl)

	network := "ip4:icmp"
	if isIPv6 {
		network = "ip6:ipv6-icmp"
	}

	res, err := net.ResolveIPAddr(network, host)
	if err != nil {
		fmt.Printf("Address resolving error: %s.\n", err)
		os.Exit(1)
	}

	p := newPingProc(net.IPAddr{IP: res.IP, Zone: res.Zone}, isIPv6, ttl)
	cn := p.getConnection(network, "")

	if err := pingLoop(p, cn); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
