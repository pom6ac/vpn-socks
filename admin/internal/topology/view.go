package topology

import (
	"net"
	"strings"

	"github.com/pom6ac/vpn-socks/admin/internal/dockerapi"
	"github.com/pom6ac/vpn-socks/admin/internal/health"
)

// Status is the availability of a node or link on the diagram.
type Status string

const (
	Up       Status = "up"
	Degraded Status = "degraded"
	Down     Status = "down"
	Unknown  Status = "unknown"
)

// Node is a box on the diagram.
type Node struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Lines  []string `json:"lines"`
	Status Status   `json:"status"`
}

// Link connects Nodes[i] and Nodes[i+1] of a Path.
type Link struct {
	Label  string `json:"label"`
	Status Status `json:"status"`
}

// Path is one horizontal chain on the diagram; len(Links) == len(Nodes)-1.
type Path struct {
	Nodes []Node `json:"nodes"`
	Links []Link `json:"links"`
}

// View is the JSON shape of /api/topology.
type View struct {
	Paths     []Path `json:"paths"`
	UpdatedAt string `json:"updated_at"`
}

// Inputs is everything Build needs. Containers holds inspect results keyed by
// name; a missing entry means the container could not be inspected.
type Inputs struct {
	OVPN, SOCKS, Admin string
	AdminListen        string
	Containers         map[string]dockerapi.ContainerInfo
	Snapshot           Snapshot
	Health             health.Report
}

// Build turns raw state into the diagram: the proxied traffic path and the
// admin panel path.
func Build(in Inputs) View {
	return View{
		Paths:     []Path{tunnelPath(in), adminPath(in)},
		UpdatedAt: in.Snapshot.UpdatedAt,
	}
}

func tunnelPath(in Inputs) Path {
	ovpn, ovpnOK := in.Containers[in.OVPN]
	socks, socksOK := in.Containers[in.SOCKS]
	snap := in.Snapshot

	// SOCKS5 shares the ovpn network namespace, so its port is published on ovpn.
	listeners, targets := hostListeners(ovpn.Ports)
	hostStatus := Unknown
	if snap.ListenerChecked {
		hostStatus = boolStatus(snap.ListenerOK)
	}
	host := Node{ID: "host", Title: "Host", Lines: orDash(listeners), Status: hostStatus}

	socksLines := []string{"SOCKS5 " + strings.Join(orDash(targets), " ")}
	if strings.HasPrefix(socks.NetworkMode, "container:") {
		socksLines = append(socksLines, "netns "+in.OVPN)
	}
	socksNode := Node{ID: "socks", Title: in.SOCKS, Lines: socksLines, Status: containerStatus(socks, socksOK)}

	ovpnStatus := containerStatus(ovpn, ovpnOK)
	if ovpnStatus == Up && snap.TunnelAddr == "" {
		ovpnStatus = Degraded
	}
	tun := snap.TunnelAddr
	if tun == "" {
		tun = "—"
	}
	ovpnNode := Node{ID: "ovpn", Title: in.OVPN, Lines: []string{"tun0 " + tun}, Status: ovpnStatus}

	tunnel := probeStatus(in.Health, "tunnel_ping")
	var serverLines []string
	transport := ""
	for _, r := range snap.Remotes {
		serverLines = append(serverLines, net.JoinHostPort(r.Host, r.Port))
	}
	if len(snap.Remotes) > 0 {
		transport = snap.Remotes[0].Proto + "/" + snap.Remotes[0].Port
	}
	server := Node{ID: "server", Title: "VPN server", Lines: orDash(serverLines), Status: tunnel}

	internetLines := []string{"exit —"}
	if snap.ExitIP != "" {
		internetLines = []string{"exit " + snap.ExitIP}
	}
	internet := Node{ID: "internet", Title: "Internet", Lines: internetLines,
		Status: combine(probeStatus(in.Health, "dns_resolve"), probeStatus(in.Health, "http_check"))}

	nodes := []Node{host, socksNode, ovpnNode, server, internet}
	labels := []string{"forward", "", transport, ""}
	return Path{Nodes: nodes, Links: linksFor(nodes, labels)}
}

func adminPath(in Inputs) Path {
	admin, ok := in.Containers[in.Admin]
	listeners, targets := hostListeners(admin.Ports)
	if !ok || len(targets) == 0 {
		targets = []string{in.AdminListen}
	}
	// The page was served by admin, so both ends are up by definition.
	nodes := []Node{
		{ID: "browser", Title: "Browser", Lines: orDash(listeners), Status: Up},
		{ID: "admin", Title: in.Admin, Lines: targets, Status: Up},
	}
	return Path{Nodes: nodes, Links: linksFor(nodes, []string{"forward"})}
}

// hostListeners formats published ports as host-side "ip:port" listeners and
// container-side ":port/proto" targets.
func hostListeners(ports []dockerapi.PortBinding) (listeners, targets []string) {
	seen := map[string]bool{}
	for _, p := range ports {
		ip := p.HostIP
		if ip == "" {
			ip = "0.0.0.0"
		}
		listeners = append(listeners, net.JoinHostPort(ip, p.HostPort))
		t := ":" + p.ContainerPort
		if p.Proto != "" && p.Proto != "tcp" {
			t += "/" + p.Proto
		}
		if !seen[t] {
			seen[t] = true
			targets = append(targets, t)
		}
	}
	return listeners, targets
}

func linksFor(nodes []Node, labels []string) []Link {
	links := make([]Link, len(nodes)-1)
	for i := range links {
		var label string
		if i < len(labels) {
			label = labels[i]
		}
		links[i] = Link{Label: label, Status: worst(nodes[i].Status, nodes[i+1].Status)}
	}
	return links
}

func containerStatus(c dockerapi.ContainerInfo, ok bool) Status {
	switch {
	case !ok:
		return Unknown
	case c.State == "running":
		return Up
	case c.State == "restarting":
		return Degraded
	default:
		return Down
	}
}

func probeStatus(r health.Report, name string) Status {
	for _, p := range r.Probes {
		if p.Name == name && p.LastRun != "" {
			return boolStatus(p.OK)
		}
	}
	return Unknown
}

func boolStatus(ok bool) Status {
	if ok {
		return Up
	}
	return Down
}

// combine merges independent checks of one node: all up -> up, all down ->
// down, mixed -> degraded. Unknown checks are ignored.
func combine(ss ...Status) Status {
	up, down := 0, 0
	for _, s := range ss {
		switch s {
		case Up:
			up++
		case Down:
			down++
		case Degraded:
			return Degraded
		}
	}
	switch {
	case up > 0 && down > 0:
		return Degraded
	case down > 0:
		return Down
	case up > 0:
		return Up
	}
	return Unknown
}

var severity = map[Status]int{Up: 0, Unknown: 1, Degraded: 2, Down: 3}

// worst returns the more severe status: a link is only as good as its ends.
func worst(a, b Status) Status {
	if severity[b] > severity[a] {
		return b
	}
	return a
}

func orDash(s []string) []string {
	if len(s) == 0 {
		return []string{"—"}
	}
	return s
}
