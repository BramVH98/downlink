/*
This package implements a syslog receiver that listens for syslog messages over TCP and UDP. It provides functionality to start and
stop the listener, handle incoming messages, and process them as needed.
*/
package syslogrecv

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

// Message represents a syslog message received by the listener.
type Message struct {
	Host      string
	Tag       string
	Severity  string
	Facility  int
	Message   string
	Timestamp time.Time
}

var severityNames = map[int]string{
	0: "error", // emergency
	1: "error", // alert
	2: "error", // critical
	3: "error", // error
	4: "warn",  // warning
	5: "info",  // notice
	6: "info",  // informational
	7: "debug", // debug
}

// Listener represents a syslog listener that can receive messages over TCP and UDP.
func Listen(addr string, allowedCIDRs []string, handle func(Message)) error {
	nets, err := parseCIDRs(allowedCIDRs)
	if err != nil {
		return fmt.Errorf("syslogrecv: %w", err)
	}

	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("syslogrecv: listen on %s: %w", addr, err)
	}
	defer conn.Close()

	log.Printf("syslog receiver listening on %s (UDP, unauthenticated by protocol)", addr)
	if len(nets) > 0 {
		log.Printf("syslog receiver: restricted to %d allowed CIDR(s)", len(nets))
	}

	buf := make([]byte, 8192)
	for {
		n, sender, err := conn.ReadFrom(buf)
		if err != nil {
			return fmt.Errorf("syslogrecv: read: %w", err)
		}

		if len(nets) > 0 && !sourceAllowed(sender, nets) {
			continue
		}

		raw := string(buf[:n])
		sourceIP := sourceIPOf(sender)
		msg, err := Parse(raw, sourceIP)
		if err != nil {
			continue
		}
		handle(msg)
	}
}

func sourceIPOf(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

func parseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if strings.Contains(c, ":") {
				c += "/128"
			} else {
				c += "/32"
			}
		}
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

func sourceAllowed(addr net.Addr, nets []*net.IPNet) bool {
	ipStr := sourceIPOf(addr)
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, net := range nets {
		if net.Contains(ip) {
			return true
		}
	}
	return false
}

// Parse handles both common syslog wire formats
func Parse(raw string, sourceIP string) (Message, error) {
	raw = strings.TrimRight(raw, "\r\n")

	facility, severity, rest, err := parsePriority(raw)
	if err != nil {
		return Message{}, err
	}

	if strings.HasPrefix(rest, "1 ") {
		return parse5425(rest[2:], facility, severity, sourceIP)
	}
	return parse3164(rest, facility, severity, sourceIP)
}

func parsePriority(raw string) (facility int, severity int, rest string, err error) {
	if !strings.HasPrefix(raw, "<") {
		return 0, 0, "", fmt.Errorf("no leading '<PRI>'")
	}
	end := strings.IndexByte(raw, '>')
	if end < 2 {
		return 0, 0, "", fmt.Errorf("malformed priority")
	}
	pri, err := strconv.Atoi(raw[1:end])
	if err != nil {
		return 0, 0, "", fmt.Errorf("non-numeric priority: %w", err)
	}
	facility = pri / 8
	severity = pri % 8
	return facility, severity, raw[end+1:], nil
}

func parse3164(rest string, facility, severity int, sourceIP string) (Message, error) {
	fields := strings.SplitN(rest, " ", 5)
	var cleaned []string
	for _, f := range fields {
		if f != "" {
			cleaned = append(cleaned, f)
		}
	}
	fields = cleaned

	host := sourceIP
	tag := "-"
	message := rest

	if len(fields) >= 4 {
		host = fields[3]
		if len(fields) >= 5 {
			message = fields[4]
		} else {
			message = ""
		}
	}

	if idx := strings.Index(message, ": "); idx != -1 {
		possibleTag := message[:idx]
		if !strings.Contains(possibleTag, " ") {
			tag = possibleTag
			message = message[idx+2:]
		}
	}

	return Message{
		Host: host, Tag: tag, Severity: severityNames[severity],
		Facility: facility, Message: message, Timestamp: time.Now(),
	}, nil
}

func parse5425(rest string, facility, severity int, sourceIP string) (Message, error) {
	fields := strings.SplitN(rest, " ", 6)
	host := sourceIP
	tag := "-"

	if len(fields) >= 2 && fields[1] != "-" {
		host = fields[1]
	}
	if len(fields) >= 3 && fields[2] != "-" {
		tag = fields[2]
	}

	message := ""
	if len(fields) >= 6 {
		message = skipStructuredData(fields[5])
	}

	return Message{
		Host: host, Tag: tag, Severity: severityNames[severity],
		Facility: facility, Message: message, Timestamp: time.Now(),
	}, nil
}

/*
skipStructuredData consumes the structured data portion of a syslog message and returns the remaining message content.
*/
func skipStructuredData(s string) string {
	if s == "-" {
		return ""
	}

	if strings.HasPrefix(s, "-") {
		return s[2:]
	}

	i := 0
	for i < len(s) && s[i] == '[' {
		depth := 1
		j := i + 1
		for j < len(s) && depth > 0 {
			switch s[j] {
			case '[':
				depth++
			case ']':
				depth--
			}
			j++
		}
		i = j
	}

	rest := s[i:]
	return strings.TrimPrefix(rest, " ")
}
