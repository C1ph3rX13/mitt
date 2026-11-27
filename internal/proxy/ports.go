//go:build linux

package proxy

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// parsePortRange parses the inclusive `a-b` form accepted by --port-range and
// returns every port in the interval. Errors wrap ErrPortRange.
func parsePortRange(value string) ([]int, error) {
	startText, endText, ok := strings.Cut(value, "-")
	if !ok || startText == "" || endText == "" || strings.Contains(endText, "-") {
		return nil, fmt.Errorf("%w %q: want a-b", ErrPortRange, value)
	}
	start, err := parsePort(startText)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %v", ErrPortRange, value, err)
	}
	end, err := parsePort(endText)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %v", ErrPortRange, value, err)
	}
	if start > end {
		return nil, fmt.Errorf("%w %q: start must not exceed end", ErrPortRange, value)
	}

	ports := make([]int, 0, end-start+1)
	for port := start; port <= end; port++ {
		ports = append(ports, port)
	}
	return ports, nil
}

func parsePort(text string) (int, error) {
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("port %q is not a decimal number", text)
		}
	}
	port, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("port %q is not a decimal number", text)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %d must be between 1 and 65535", port)
	}
	return port, nil
}

// listenerAddresses combines the host part of addr with each port. The host
// may be given as host:port, bare host, bare IP, or empty for the wildcard
// address; the port in addr is ignored.
func listenerAddresses(addr string, ports []int) ([]string, error) {
	host, err := listenerHost(addr)
	if err != nil {
		return nil, err
	}
	addrs := make([]string, 0, len(ports))
	for _, port := range ports {
		addrs = append(addrs, net.JoinHostPort(host, strconv.Itoa(port)))
	}
	return addrs, nil
}

// listenerHost extracts the host part of addr. Errors describe only the
// rejected part; callers wrap them with the offending --addr value.
func listenerHost(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 0 || number > 65535 {
			return "", errors.New("port must be between 0 and 65535")
		}
		return host, nil
	}
	if addr == "" {
		return "", nil
	}
	if net.ParseIP(addr) != nil {
		return addr, nil
	}
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		if ip := net.ParseIP(addr[1 : len(addr)-1]); ip != nil {
			return ip.String(), nil
		}
	}
	if !strings.Contains(addr, ":") {
		return addr, nil
	}
	return "", errors.New("want host:port, a bare host, or a bare IP")
}
