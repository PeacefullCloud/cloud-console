package instances

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/database"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// ListDedicatedIPs returns the admin pool of dedicated public IPs.
func (s *Service) ListDedicatedIPs() ([]models.DedicatedIP, error) {
	return s.app.DB.ListDedicatedIPs()
}

// AddDedicatedIP validates and adds an external IP to the admin pool.
// listenIP is optional; when empty, Incus listens directly on publicIP.
func (s *Service) AddDedicatedIP(ctx context.Context, publicIP, listenIP, network, label string) (*models.DedicatedIP, error) {
	pub, listen, netName, cleanLabel, err := s.validateDedicatedIPInput(publicIP, listenIP, network, label)
	if err != nil {
		return nil, err
	}

	effective := pub
	if listen != "" {
		effective = listen
	}

	existing, err := s.app.DB.ListDedicatedIPs()
	if err != nil {
		return nil, err
	}
	for _, row := range existing {
		if row.PublicIP == pub {
			return nil, fmt.Errorf("%w: %s is already in the pool", ErrInvalidSpec, pub)
		}
		if row.EffectiveListenIP() == effective && row.Network == netName {
			return nil, fmt.Errorf("%w: host listen IP %s is already used in the pool", ErrInvalidSpec, effective)
		}
	}

	if s.app.Incus != nil {
		netInfo, err := s.app.Incus.Network(netName)
		if err != nil {
			if incus.IsNotFound(err) {
				return nil, fmt.Errorf("%w: Incus network %q was not found", ErrInvalidSpec, netName)
			}
			return nil, err
		}
		if !netInfo.Managed || (netInfo.Type != "bridge" && netInfo.Type != "ovn") {
			return nil, fmt.Errorf("%w: network %q must be a managed bridge or OVN network", ErrInvalidSpec, netName)
		}
		if allNets, nerr := s.app.Incus.Networks(); nerr == nil {
			parsed := net.ParseIP(effective)
			for _, n := range allNets {
				for _, key := range []string{"ipv4.address", "ipv6.address"} {
					cidr := strings.TrimSpace(n.Config[key])
					if cidr == "" || cidr == "none" {
						continue
					}
					if _, ipNet, perr := net.ParseCIDR(cidr); perr == nil && ipNet.Contains(parsed) {
						return nil, fmt.Errorf("%w: %s overlaps managed network %s (%s)", ErrInvalidSpec, effective, n.Name, cidr)
					}
				}
			}
		}
		if _, _, err := s.app.Incus.NetworkForward(netName, effective); err == nil {
			return nil, fmt.Errorf("%w: Incus already has a network forward on %s for %s", ErrInvalidSpec, netName, effective)
		}
	}

	return s.app.DB.CreateDedicatedIP(pub, listen, netName, cleanLabel)
}

// RemoveDedicatedIP deletes an unassigned IP from the admin pool.
func (s *Service) RemoveDedicatedIP(ctx context.Context, id int64) error {
	row, err := s.app.DB.GetDedicatedIP(id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if row.InstanceName != "" {
		return fmt.Errorf("%w: unassign %s from %s before removing it", ErrInvalidSpec, row.PublicIP, row.InstanceName)
	}
	return s.app.DB.DeleteDedicatedIP(id)
}

// AssignDedicatedIP creates a 1:1 Incus network forward from an available pool
// IP to instanceName's private IP on the target network.
func (s *Service) AssignDedicatedIP(ctx context.Context, id int64, instanceName string) (*models.DedicatedIP, error) {
	instanceName = strings.TrimSpace(instanceName)
	if instanceName == "" {
		return nil, fmt.Errorf("%w: choose an instance", ErrInvalidSpec)
	}
	if s.hasActiveJob(instanceName) {
		return nil, ErrBusy
	}

	row, err := s.app.DB.GetDedicatedIP(id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if row.InstanceName != "" {
		return nil, fmt.Errorf("%w: %s is already assigned to %s", ErrInvalidSpec, row.PublicIP, row.InstanceName)
	}

	full, err := s.app.Incus.InstanceFull(instanceName)
	if err != nil {
		if incus.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	wantV4 := isIPv4(row.EffectiveListenIP())
	nicName, targetIP, err := findInstanceNetworkTarget(full, row.Network, wantV4)
	if err != nil {
		return nil, err
	}

	// Best-effort pin of the private IP on the NIC so DHCP leases do not
	// drift across restarts or rebuilds.
	s.pinNICAddress(ctx, &full.Instance, nicName, targetIP, wantV4)

	snat, err := s.applyNetworkForward(row.Network, row.EffectiveListenIP(), targetIP, instanceName)
	if err != nil {
		return nil, err
	}

	if err := s.app.DB.AssignDedicatedIP(row.ID, instanceName, targetIP, snat); err != nil {
		_ = s.app.Incus.DeleteNetworkForward(row.Network, row.EffectiveListenIP())
		return nil, err
	}
	return s.app.DB.GetDedicatedIP(row.ID)
}

// UnassignDedicatedIP removes the Incus network forward and returns the IP to
// the available pool.
func (s *Service) UnassignDedicatedIP(ctx context.Context, id int64) (*models.DedicatedIP, error) {
	row, err := s.app.DB.GetDedicatedIP(id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if row.InstanceName == "" {
		return row, nil
	}

	if s.app.Incus != nil {
		if err := s.app.Incus.DeleteNetworkForward(row.Network, row.EffectiveListenIP()); err != nil && !incus.IsNotFound(err) {
			return nil, err
		}
	}
	if err := s.app.DB.UnassignDedicatedIP(row.ID); err != nil {
		return nil, err
	}
	return s.app.DB.GetDedicatedIP(row.ID)
}

func (s *Service) releaseDedicatedIPs(ctx context.Context, instanceName string) {
	rows, err := s.app.DB.ListDedicatedIPsForInstance(instanceName)
	if err != nil {
		s.app.Log.Warn("could not list dedicated IPs on delete", "instance", instanceName, "err", err)
		return
	}
	for _, row := range rows {
		if s.app.Incus != nil {
			if err := s.app.Incus.DeleteNetworkForward(row.Network, row.EffectiveListenIP()); err != nil && !incus.IsNotFound(err) {
				s.app.Log.Warn("could not delete network forward", "listen_ip", row.EffectiveListenIP(), "err", err)
			}
		}
		if err := s.app.DB.UnassignDedicatedIP(row.ID); err != nil {
			s.app.Log.Warn("could not unassign dedicated IP", "id", row.ID, "err", err)
		}
	}
}

func (s *Service) renameDedicatedIPs(ctx context.Context, oldName, newName string) {
	if err := s.app.DB.RenameDedicatedIPs(oldName, newName); err != nil {
		s.app.Log.Warn("could not rename dedicated IP records", "from", oldName, "to", newName, "err", err)
		return
	}
	if s.app.Incus == nil {
		return
	}
	rows, err := s.app.DB.ListDedicatedIPsForInstance(newName)
	if err != nil {
		return
	}
	for _, row := range rows {
		fwd, etag, err := s.app.Incus.NetworkForward(row.Network, row.EffectiveListenIP())
		if err != nil {
			continue
		}
		put := fwd.Writable()
		put.Description = "cloud-console: " + newName
		_ = s.app.Incus.UpdateNetworkForward(row.Network, row.EffectiveListenIP(), put, etag)
	}
}

func (s *Service) syncDedicatedIPs(ctx context.Context, instanceName string) {
	if s.app.Incus == nil {
		return
	}
	rows, err := s.app.DB.ListDedicatedIPsForInstance(instanceName)
	if err != nil || len(rows) == 0 {
		return
	}
	full, err := s.app.Incus.InstanceFull(instanceName)
	if err != nil {
		return
	}
	for _, row := range rows {
		wantV4 := isIPv4(row.EffectiveListenIP())
		nicName, targetIP, err := findInstanceNetworkTarget(full, row.Network, wantV4)
		if err != nil || targetIP == "" {
			continue
		}
		s.pinNICAddress(ctx, &full.Instance, nicName, targetIP, wantV4)
		if targetIP == row.TargetIP {
			continue
		}
		_ = s.app.Incus.DeleteNetworkForward(row.Network, row.EffectiveListenIP())
		snat, err := s.applyNetworkForward(row.Network, row.EffectiveListenIP(), targetIP, instanceName)
		if err != nil {
			s.app.Log.Warn("could not re-sync dedicated IP forward", "instance", instanceName, "ip", row.PublicIP, "err", err)
			continue
		}
		_ = s.app.DB.AssignDedicatedIP(row.ID, instanceName, targetIP, snat)
	}
}

func (s *Service) applyNetworkForward(network, listenIP, targetIP, instanceName string) (bool, error) {
	desc := "cloud-console: " + instanceName

	// Primary mode: full TCP+UDP port range with SNAT so both inbound and
	// outbound traffic from the instance use the dedicated IP.
	post := incusapi.NetworkForwardsPost{
		ListenAddress: listenIP,
		NetworkForwardPut: incusapi.NetworkForwardPut{
			Description: desc,
			Ports: []incusapi.NetworkForwardPort{
				{
					Protocol:      "tcp",
					ListenPort:    "1-65535",
					TargetPort:    "1-65535",
					TargetAddress: targetIP,
					SNAT:          true,
				},
				{
					Protocol:      "udp",
					ListenPort:    "1-65535",
					TargetPort:    "1-65535",
					TargetAddress: targetIP,
					SNAT:          true,
				},
			},
		},
	}
	if err := s.app.Incus.CreateNetworkForward(network, post); err == nil {
		return true, nil
	} else if isSNATUnsupportedErr(err) {
		// Fallback for hosts using xtables or OVN where Incus SNAT on bridge
		// port forwards is unavailable: inbound 1:1 forward via target_address.
		fallback := incusapi.NetworkForwardsPost{
			ListenAddress: listenIP,
			NetworkForwardPut: incusapi.NetworkForwardPut{
				Description: desc,
				Config:      map[string]string{"target_address": targetIP},
			},
		}
		if ferr := s.app.Incus.CreateNetworkForward(network, fallback); ferr != nil {
			return false, ferr
		}
		return false, nil
	} else {
		return false, err
	}
}

func isSNATUnsupportedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "snat") || strings.Contains(msg, "nftables") || strings.Contains(msg, "xtables")
}

func (s *Service) pinNICAddress(ctx context.Context, inst *incusapi.Instance, nicName, targetIP string, isV4 bool) {
	if inst == nil || nicName == "" || targetIP == "" {
		return
	}
	key := "ipv6.address"
	if isV4 {
		key = "ipv4.address"
	}
	if inst.ExpandedDevices[nicName][key] == targetIP {
		return
	}
	put := cloneInstancePut(inst)
	nic := map[string]string{}
	for k, v := range inst.ExpandedDevices[nicName] {
		nic[k] = v
	}
	nic[key] = targetIP
	put.Devices[nicName] = nic
	if err := s.app.Incus.UpdateInstance(ctx, inst.Name, put); err != nil {
		s.app.Log.Debug("could not pin NIC private address", "instance", inst.Name, "nic", nicName, "ip", targetIP, "err", err)
	}
}

func findInstanceNetworkTarget(full *incusapi.InstanceFull, network string, wantV4 bool) (string, string, error) {
	if full == nil {
		return "", "", ErrNotFound
	}
	familyLabel := "IPv6"
	staticKey := "ipv6.address"
	if wantV4 {
		familyLabel = "IPv4"
		staticKey = "ipv4.address"
	}

	var nicName string
	var nicCfg map[string]string
	for name, dev := range full.ExpandedDevices {
		if dev["type"] == "nic" && dev["network"] == network {
			nicName = name
			nicCfg = dev
			break
		}
	}
	if nicName == "" {
		return "", "", fmt.Errorf("%w: instance %s has no NIC attached to network %s", ErrInvalidSpec, full.Name, network)
	}

	// Prefer a live global address on that NIC. Match by NIC name first, then
	// by volatile MAC address (VMs name interfaces enp5s0 etc.).
	if full.State != nil {
		hwaddr := full.ExpandedConfig["volatile."+nicName+".hwaddr"]
		if addr := matchingInterfaceIP(full.State.Network[nicName], wantV4); addr != "" {
			return nicName, addr, nil
		}
		if hwaddr != "" {
			for _, iface := range full.State.Network {
				if strings.EqualFold(iface.Hwaddr, hwaddr) {
					if addr := matchingInterfaceIP(iface, wantV4); addr != "" {
						return nicName, addr, nil
					}
				}
			}
		}
	}

	// Fall back to a static address pinned on the NIC (works even when stopped).
	if staticAddr := strings.TrimSpace(nicCfg[staticKey]); staticAddr != "" {
		if parsed := net.ParseIP(staticAddr); parsed != nil && (parsed.To4() != nil) == wantV4 {
			return nicName, parsed.String(), nil
		}
	}

	return "", "", fmt.Errorf("%w: %s has no private %s address on %s yet (start the instance first)", ErrInvalidSpec, full.Name, familyLabel, network)
}

func matchingInterfaceIP(iface incusapi.InstanceStateNetwork, wantV4 bool) string {
	for _, a := range iface.Addresses {
		if a.Scope != "global" || a.Address == "" {
			continue
		}
		parsed := net.ParseIP(a.Address)
		if parsed == nil || parsed.IsLoopback() {
			continue
		}
		if (parsed.To4() != nil) == wantV4 {
			return parsed.String()
		}
	}
	return ""
}

func (s *Service) validateDedicatedIPInput(publicIP, listenIP, network, label string) (string, string, string, string, error) {
	pubRaw := strings.TrimSpace(publicIP)
	pubParsed := parseAssignableIP(pubRaw)
	if pubParsed == nil {
		return "", "", "", "", fmt.Errorf("%w: enter a valid public IPv4 or IPv6 address", ErrInvalidSpec)
	}
	pub := pubParsed.String()

	listen := ""
	if listenRaw := strings.TrimSpace(listenIP); listenRaw != "" {
		listenParsed := parseAssignableIP(listenRaw)
		if listenParsed == nil {
			return "", "", "", "", fmt.Errorf("%w: host listen IP must be a valid IPv4 or IPv6 address", ErrInvalidSpec)
		}
		if (listenParsed.To4() != nil) != (pubParsed.To4() != nil) {
			return "", "", "", "", fmt.Errorf("%w: public IP and host listen IP must both be IPv4 or both be IPv6", ErrInvalidSpec)
		}
		if listenParsed.String() != pub {
			listen = listenParsed.String()
		}
	}

	netName := strings.TrimSpace(network)
	if netName == "" {
		netName = "incusbr0"
	}

	cleanLabel := strings.TrimSpace(label)
	if len(cleanLabel) > 64 {
		cleanLabel = cleanLabel[:64]
	}

	return pub, listen, netName, cleanLabel, nil
}

func parseAssignableIP(raw string) net.IP {
	ip := net.ParseIP(raw)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return nil
	}
	return ip
}

func isIPv4(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.To4() != nil
}
