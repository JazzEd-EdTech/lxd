package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/cluster/request"
	"github.com/lxc/lxd/lxd/db"
	dbCluster "github.com/lxc/lxd/lxd/db/cluster"
	"github.com/lxc/lxd/lxd/network/acl"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/resources"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/validate"
	"github.com/lxc/lxd/shared/version"
)

// Info represents information about a network driver.
type Info struct {
	Projects           bool // Indicates if driver can be used in network enabled projects.
	NodeSpecificConfig bool // Whether driver has cluster node specific config as a prerequisite for creation.
	AddressForwards    bool // Indicates if driver supports address forwards.
	Peering            bool // Indicates if the driver supports network peering.
}

// forwardPortMap represents a mapping of listen port(s) to target port(s) for a protocol/target address pair.
type forwardPortMap struct {
	listenPorts   []uint64
	targetPorts   []uint64
	targetAddress net.IP
	protocol      string
}

// externalSubnetUsage represents usage of a subnet by a network or NIC.
type externalSubnetUsage struct {
	subnet          net.IPNet
	networkProject  string
	networkName     string
	networkSNAT     bool
	instanceProject string
	instanceName    string
	instanceDevice  string
}

// common represents a generic LXD network.
type common struct {
	logger      logger.Logger
	state       *state.State
	id          int64
	project     string
	name        string
	description string
	config      map[string]string
	status      string
	managed     bool
	nodes       map[int64]db.NetworkNode
}

// init initialise internal variables.
func (n *common) init(state *state.State, id int64, projectName string, netInfo *api.Network, netNodes map[int64]db.NetworkNode) {
	n.logger = logger.AddContext(logger.Log, logger.Ctx{"project": projectName, "driver": netInfo.Type, "network": netInfo.Name})
	n.id = id
	n.project = projectName
	n.name = netInfo.Name
	n.config = netInfo.Config
	n.state = state
	n.description = netInfo.Description
	n.status = netInfo.Status
	n.managed = netInfo.Managed
	n.nodes = netNodes
}

// FillConfig fills requested config with any default values, by default this is a no-op.
func (n *common) FillConfig(config map[string]string) error {
	return nil
}

// validationRules returns a map of config rules common to all drivers.
func (n *common) validationRules() map[string]func(string) error {
	return map[string]func(string) error{}
}

// validate a network config against common rules and optional driver specific rules.
func (n *common) validate(config map[string]string, driverRules map[string]func(value string) error) error {
	checkedFields := map[string]struct{}{}

	// Get rules common for all drivers.
	rules := n.validationRules()

	// Merge driver specific rules into common rules.
	for field, validator := range driverRules {
		rules[field] = validator
	}

	// Run the validator against each field.
	for k, validator := range rules {
		checkedFields[k] = struct{}{} //Mark field as checked.
		err := validator(config[k])
		if err != nil {
			return fmt.Errorf("Invalid value for network %q option %q: %w", n.name, k, err)
		}
	}

	// Look for any unchecked fields, as these are unknown fields and validation should fail.
	for k := range config {
		_, checked := checkedFields[k]
		if checked {
			continue
		}

		// User keys are not validated.
		if shared.IsUserConfig(k) {
			continue
		}

		return fmt.Errorf("Invalid option for network %q option %q", n.name, k)
	}

	return nil
}

// validateZoneName checks that a user provided zone name is valid.
func (n *common) validateZoneName(name string) error {
	_, _, _, err := n.state.DB.Cluster.GetNetworkZone(name)
	if err != nil {
		return fmt.Errorf("Network zone %q doesn't exist", name)
	}

	return nil
}

// ValidateName validates network name.
func (n *common) ValidateName(name string) error {
	err := validate.IsURLSegmentSafe(name)
	if err != nil {
		return err
	}

	if strings.Contains(name, ":") {
		return fmt.Errorf("Cannot contain %q", ":")
	}

	return nil
}

// ID returns the network ID.
func (n *common) ID() int64 {
	return n.id
}

// Name returns the network name.
func (n *common) Name() string {
	return n.name
}

// Project returns the network project.
func (n *common) Project() string {
	return n.project
}

// Description returns the network description.
func (n *common) Description() string {
	return n.description
}

// Status returns the network status.
func (n *common) Status() string {
	return n.status
}

// LocalStatus returns network status of the local cluster member.
func (n *common) LocalStatus() string {
	// Check if network is unavailable locally and replace status if so.
	if !IsAvailable(n.Project(), n.Name()) {
		return api.NetworkStatusUnavailable
	}

	node, exists := n.nodes[n.state.DB.Cluster.GetNodeID()]
	if !exists {
		return api.NetworkStatusUnknown
	}

	return db.NetworkStateToAPIStatus(node.State)
}

// Config returns the network config.
func (n *common) Config() map[string]string {
	return n.config
}

func (n *common) IsManaged() bool {
	return n.managed
}

// Config returns the common network driver info.
func (n *common) Info() Info {
	return Info{
		Projects:           false,
		NodeSpecificConfig: true,
		AddressForwards:    false,
	}
}

// Locations returns the list of cluster members this network is configured on.
func (n *common) Locations() []string {
	locations := make([]string, 0, len(n.nodes))
	for _, netNode := range n.nodes {
		locations = append(locations, netNode.Name)
	}

	return locations
}

// IsUsed returns whether the network is used by any instances or profiles.
func (n *common) IsUsed() (bool, error) {
	usedBy, err := UsedBy(n.state, n.project, n.id, n.name, true)
	if err != nil {
		return false, err
	}

	return len(usedBy) > 0, nil
}

// DHCPv4Subnet returns nil always.
func (n *common) DHCPv4Subnet() *net.IPNet {
	return nil
}

// DHCPv6Subnet returns nil always.
func (n *common) DHCPv6Subnet() *net.IPNet {
	return nil
}

// DHCPv4Ranges returns a parsed set of DHCPv4 ranges for this network.
func (n *common) DHCPv4Ranges() []shared.IPRange {
	dhcpRanges := make([]shared.IPRange, 0)
	if n.config["ipv4.dhcp.ranges"] != "" {
		for _, r := range strings.Split(n.config["ipv4.dhcp.ranges"], ",") {
			parts := strings.SplitN(strings.TrimSpace(r), "-", 2)
			if len(parts) == 2 {
				startIP := net.ParseIP(parts[0])
				endIP := net.ParseIP(parts[1])
				dhcpRanges = append(dhcpRanges, shared.IPRange{
					Start: startIP.To4(),
					End:   endIP.To4(),
				})
			}
		}
	}

	return dhcpRanges
}

// DHCPv6Ranges returns a parsed set of DHCPv6 ranges for this network.
func (n *common) DHCPv6Ranges() []shared.IPRange {
	dhcpRanges := make([]shared.IPRange, 0)
	if n.config["ipv6.dhcp.ranges"] != "" {
		for _, r := range strings.Split(n.config["ipv6.dhcp.ranges"], ",") {
			parts := strings.SplitN(strings.TrimSpace(r), "-", 2)
			if len(parts) == 2 {
				startIP := net.ParseIP(parts[0])
				endIP := net.ParseIP(parts[1])
				dhcpRanges = append(dhcpRanges, shared.IPRange{
					Start: startIP.To16(),
					End:   endIP.To16(),
				})
			}
		}
	}

	return dhcpRanges
}

// update the internal config variables, and if not cluster notification, notifies all nodes and updates database.
func (n *common) update(applyNetwork api.NetworkPut, targetNode string, clientType request.ClientType) error {
	// Update internal config before database has been updated (so that if update is a notification we apply
	// the config being supplied and not that in the database).
	n.description = applyNetwork.Description
	n.config = applyNetwork.Config

	// If this update isn't coming via a cluster notification itself, then notify all nodes of change and then
	// update the database.
	if clientType != request.ClientTypeNotifier {
		if targetNode == "" {
			// Notify all other nodes to update the network if no target specified.
			notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
			if err != nil {
				return err
			}

			sendNetwork := applyNetwork
			sendNetwork.Config = make(map[string]string)
			for k, v := range applyNetwork.Config {
				// Don't forward node specific keys (these will be merged in on recipient node).
				if shared.StringInSlice(k, db.NodeSpecificNetworkConfig) {
					continue
				}

				sendNetwork.Config[k] = v
			}

			err = notifier(func(client lxd.InstanceServer) error {
				return client.UseProject(n.project).UpdateNetwork(n.name, sendNetwork, "")
			})
			if err != nil {
				return err
			}
		}

		// Update the database.
		err := n.state.DB.Cluster.UpdateNetwork(n.project, n.name, applyNetwork.Description, applyNetwork.Config)
		if err != nil {
			return err
		}
	}

	return nil
}

// configChanged compares supplied new config with existing config. Returns a boolean indicating if differences in
// the config or description were found (and the database record needs updating), and a list of non-user config
// keys that have changed, and a copy of the current internal network config that can be used to revert if needed.
func (n *common) configChanged(newNetwork api.NetworkPut) (bool, []string, api.NetworkPut, error) {
	// Backup the current state.
	oldNetwork := api.NetworkPut{
		Description: n.description,
		Config:      map[string]string{},
	}

	err := shared.DeepCopy(&n.config, &oldNetwork.Config)
	if err != nil {
		return false, nil, oldNetwork, err
	}

	// Diff the configurations.
	changedKeys := []string{}
	dbUpdateNeeded := false

	if newNetwork.Description != n.description {
		dbUpdateNeeded = true
	}

	for k, v := range oldNetwork.Config {
		if v != newNetwork.Config[k] {
			dbUpdateNeeded = true

			// Add non-user changed key to list of changed keys.
			if !strings.HasPrefix(k, "user.") && !shared.StringInSlice(k, changedKeys) {
				changedKeys = append(changedKeys, k)
			}
		}
	}

	for k, v := range newNetwork.Config {
		if v != oldNetwork.Config[k] {
			dbUpdateNeeded = true

			// Add non-user changed key to list of changed keys.
			if !strings.HasPrefix(k, "user.") && !shared.StringInSlice(k, changedKeys) {
				changedKeys = append(changedKeys, k)
			}
		}
	}

	return dbUpdateNeeded, changedKeys, oldNetwork, nil
}

// rename the network directory, update database record and update internal variables.
func (n *common) rename(newName string) error {
	// Clear new directory if exists.
	if shared.PathExists(shared.VarPath("networks", newName)) {
		_ = os.RemoveAll(shared.VarPath("networks", newName))
	}

	// Rename directory to new name.
	if shared.PathExists(shared.VarPath("networks", n.name)) {
		err := os.Rename(shared.VarPath("networks", n.name), shared.VarPath("networks", newName))
		if err != nil {
			return err
		}
	}

	// Rename the database entry.
	err := n.state.DB.Cluster.RenameNetwork(n.project, n.name, newName)
	if err != nil {
		return err
	}

	// Reinitialise internal name variable and logger context with new name.
	n.name = newName

	return nil
}

// warningsDelete deletes any persistent warnings for the network.
func (n *common) warningsDelete() error {
	err := n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		return tx.DeleteWarnings(dbCluster.TypeNetwork, int(n.ID()))
	})
	if err != nil {
		return fmt.Errorf("Failed deleting persistent warnings: %w", err)
	}

	return nil
}

// delete the network on local server.
func (n *common) delete(clientType request.ClientType) error {
	// Delete any persistent warnings for network.
	err := n.warningsDelete()
	if err != nil {
		return err
	}

	// Cleanup storage.
	if shared.PathExists(shared.VarPath("networks", n.name)) {
		_ = os.RemoveAll(shared.VarPath("networks", n.name))
	}

	pn := ProjectNetwork{
		ProjectName: n.Project(),
		NetworkName: n.Name(),
	}

	unavailableNetworksMu.Lock()
	delete(unavailableNetworks, pn)
	unavailableNetworksMu.Unlock()

	return nil
}

// Create is a no-op.
func (n *common) Create(clientType request.ClientType) error {
	n.logger.Debug("Create", logger.Ctx{"clientType": clientType, "config": n.config})
	return nil
}

// HandleHeartbeat is a no-op.
func (n *common) HandleHeartbeat(heartbeatData *cluster.APIHeartbeat) error {
	return nil
}

// notifyDependentNetworks allows any dependent networks to apply changes to themselves when this network changes.
func (n *common) notifyDependentNetworks(changedKeys []string) {
	if n.Project() != project.Default {
		return // Only networks in the default project can be used as dependent networks.
	}

	// Get a list of projects.
	var err error
	var projectNames []string

	err = n.state.DB.Cluster.Transaction(context.TODO(), func(ctx context.Context, tx *db.ClusterTx) error {
		projectNames, err = dbCluster.GetProjectNames(ctx, tx.Tx())
		return err
	})
	if err != nil {
		n.logger.Error("Failed to load projects", logger.Ctx{"err": err})
		return
	}

	for _, projectName := range projectNames {
		// Get a list of managed networks in project.
		depNets, err := n.state.DB.Cluster.GetCreatedNetworks(projectName)
		if err != nil {
			n.logger.Error("Failed to load networks in project", logger.Ctx{"project": projectName, "err": err})
			continue // Continue to next project.
		}

		for _, depName := range depNets {
			depNet, err := LoadByName(n.state, projectName, depName)
			if err != nil {
				n.logger.Error("Failed to load dependent network", logger.Ctx{"project": projectName, "dependentNetwork": depName, "err": err})
				continue // Continue to next network.
			}

			if depNet.Config()["network"] != n.Name() {
				continue // Skip network, as does not depend on our network.
			}

			err = depNet.handleDependencyChange(n.Name(), n.Config(), changedKeys)
			if err != nil {
				n.logger.Error("Failed notifying dependent network", logger.Ctx{"project": projectName, "dependentNetwork": depName, "err": err})
				continue // Continue to next network.
			}
		}
	}
}

// handleDependencyChange is a placeholder for networks that don't need to handle changes from dependent networks.
func (n *common) handleDependencyChange(netName string, netConfig map[string]string, changedKeys []string) error {
	return nil
}

// bgpValidate
func (n *common) bgpValidationRules(config map[string]string) (map[string]func(value string) error, error) {
	rules := map[string]func(value string) error{}
	for k := range config {
		// BGP keys have the peer name in their name, extract the suffix.
		if !strings.HasPrefix(k, "bgp.") {
			continue
		}

		// Validate remote name in key.
		fields := strings.Split(k, ".")
		if len(fields) != 4 {
			return nil, fmt.Errorf("Invalid network configuration key: %s", k)
		}

		bgpKey := fields[3]

		// Add the correct validation rule for the dynamic field based on last part of key.
		switch bgpKey {
		case "address":
			rules[k] = validate.Optional(validate.IsNetworkAddress)
		case "asn":
			rules[k] = validate.Optional(validate.IsInRange(1, 4294967294))
		case "password":
			rules[k] = validate.Optional(validate.IsAny)
		}
	}

	return rules, nil
}

// bgpSetup initializes BGP peers and prefixes.
func (n *common) bgpSetup(oldConfig map[string]string) error {
	err := n.bgpSetupPeers(oldConfig)
	if err != nil {
		return fmt.Errorf("Failed setting up BGP peers: %w", err)
	}

	err = n.bgpSetupPrefixes(oldConfig)
	if err != nil {
		return fmt.Errorf("Failed setting up BGP prefixes: %w", err)
	}

	// Refresh exported BGP prefixes on local member.
	err = n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	return nil
}

// bgpClear initializes BGP peers and prefixes.
func (n *common) bgpClear(config map[string]string) error {
	// Clear all peers.
	err := n.bgpClearPeers(config)
	if err != nil {
		return err
	}

	// Clear all prefixes.
	err = n.state.BGP.RemovePrefixByOwner(fmt.Sprintf("network_%d", n.id))
	if err != nil {
		return err
	}

	// Clear existing address forward prefixes for network.
	err = n.state.BGP.RemovePrefixByOwner(fmt.Sprintf("network_%d_forward", n.id))
	if err != nil {
		return err
	}

	return nil
}

// bgpClearPeers removes all BGP peers on the network.
func (n *common) bgpClearPeers(config map[string]string) error {
	peers := n.bgpGetPeers(config)
	for _, peer := range peers {
		// Remove the peer.
		fields := strings.Split(peer, ",")
		err := n.state.BGP.RemovePeer(net.ParseIP(fields[0]))
		if err != nil {
			return err
		}
	}

	return nil
}

// bgpSetupPeers updates the list of BGP peers.
func (n *common) bgpSetupPeers(oldConfig map[string]string) error {
	// Setup BGP (and handled config changes).
	newPeers := n.bgpGetPeers(n.config)
	oldPeers := n.bgpGetPeers(oldConfig)

	// Remove old peers.
	for _, peer := range oldPeers {
		if shared.StringInSlice(peer, newPeers) {
			continue
		}

		// Remove old peer.
		fields := strings.Split(peer, ",")
		err := n.state.BGP.RemovePeer(net.ParseIP(fields[0]))
		if err != nil {
			return err
		}
	}

	// Add new peers.
	for _, peer := range newPeers {
		if shared.StringInSlice(peer, oldPeers) {
			continue
		}

		// Add new peer.
		fields := strings.Split(peer, ",")
		asn, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			return err
		}

		err = n.state.BGP.AddPeer(net.ParseIP(fields[0]), uint32(asn), fields[2])
		if err != nil {
			return err
		}
	}

	return nil
}

// bgpNextHopAddress parses nexthop configuration and returns next hop address to use for BGP routes.
// Uses first of bgp.ipv{ipVersion}.nexthop or volatile.network.ipv{ipVersion}.address or wildcard address.
func (n *common) bgpNextHopAddress(ipVersion uint) net.IP {
	nextHopAddr := net.ParseIP(n.config[fmt.Sprintf("bgp.ipv%d.nexthop", ipVersion)])
	if nextHopAddr == nil {
		nextHopAddr = net.ParseIP(n.config[fmt.Sprintf("volatile.network.ipv%d.address", ipVersion)])
		if nextHopAddr == nil {
			if ipVersion == 4 {
				nextHopAddr = net.ParseIP("0.0.0.0")
			} else {
				nextHopAddr = net.ParseIP("::")
			}
		}
	}

	return nextHopAddr
}

// bgpSetupPrefixes refreshes the prefix list for the network.
func (n *common) bgpSetupPrefixes(oldConfig map[string]string) error {
	// Clear existing prefixes.
	bgpOwner := fmt.Sprintf("network_%d", n.id)
	if oldConfig != nil {
		err := n.state.BGP.RemovePrefixByOwner(bgpOwner)
		if err != nil {
			return err
		}
	}

	// Add the new prefixes.
	for _, ipVersion := range []uint{4, 6} {
		nextHopAddr := n.bgpNextHopAddress(ipVersion)

		// If network has NAT enabled, then export network's NAT address if specified.
		if shared.IsTrue(n.config[fmt.Sprintf("ipv%d.nat", ipVersion)]) {
			natAddressKey := fmt.Sprintf("ipv%d.nat.address", ipVersion)
			if n.config[natAddressKey] != "" {
				subnetSize := 128
				if ipVersion == 4 {
					subnetSize = 32
				}

				_, subnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", n.config[natAddressKey], subnetSize))
				if err != nil {
					return err
				}

				err = n.state.BGP.AddPrefix(*subnet, nextHopAddr, bgpOwner)
				if err != nil {
					return err
				}
			}
		} else if !shared.StringInSlice(n.config[fmt.Sprintf("ipv%d.address", ipVersion)], []string{"", "none"}) {
			// If network has NAT disabled, then export the network's subnet if specified.
			netAddress := n.config[fmt.Sprintf("ipv%d.address", ipVersion)]
			_, subnet, err := net.ParseCIDR(netAddress)
			if err != nil {
				return fmt.Errorf("Failed parsing network address %q: %w", netAddress, err)
			}

			err = n.state.BGP.AddPrefix(*subnet, nextHopAddr, bgpOwner)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// bgpGetPeers returns a list of strings representing the BGP peers.
func (n *common) bgpGetPeers(config map[string]string) []string {
	// Get a list of peer names.
	peerNames := []string{}
	for k := range config {
		if !strings.HasPrefix(k, "bgp.peers.") {
			continue
		}

		fields := strings.Split(k, ".")
		if !shared.StringInSlice(fields[2], peerNames) {
			peerNames = append(peerNames, fields[2])
		}
	}

	// Build up a list of peer strings.
	peers := []string{}
	for _, peerName := range peerNames {
		peerAddress := config[fmt.Sprintf("bgp.peers.%s.address", peerName)]
		peerASN := config[fmt.Sprintf("bgp.peers.%s.asn", peerName)]
		peerPassword := config[fmt.Sprintf("bgp.peers.%s.password", peerName)]

		if peerAddress != "" && peerASN != "" {
			peers = append(peers, fmt.Sprintf("%s,%s,%s", peerAddress, peerASN, peerPassword))
		}
	}

	return peers
}

// forwardValidate valites the forward request.
func (n *common) forwardValidate(listenAddress net.IP, forward *api.NetworkForwardPut) ([]*forwardPortMap, error) {
	if listenAddress == nil {
		return nil, fmt.Errorf("Invalid listen address")
	}

	listenIsIP4 := listenAddress.To4() != nil

	// For checking target addresses are within network's subnet.
	netIPKey := "ipv4.address"
	if !listenIsIP4 {
		netIPKey = "ipv6.address"
	}

	netIPAddress := n.config[netIPKey]

	var err error
	var netSubnet *net.IPNet
	if netIPAddress != "" {
		_, netSubnet, err = net.ParseCIDR(n.config[netIPKey])
		if err != nil {
			return nil, err
		}
	}

	// Look for any unknown config fields.
	for k := range forward.Config {
		if k == "target_address" {
			continue
		}

		// User keys are not validated.
		if shared.IsUserConfig(k) {
			continue
		}

		return nil, fmt.Errorf("Invalid option %q", k)
	}

	// Validate default target address.
	defaultTargetAddress := net.ParseIP(forward.Config["target_address"])

	if forward.Config["target_address"] != "" {
		if defaultTargetAddress == nil {
			return nil, fmt.Errorf("Invalid default target address")
		}

		defaultTargetIsIP4 := defaultTargetAddress.To4() != nil
		if listenIsIP4 != defaultTargetIsIP4 {
			return nil, fmt.Errorf("Cannot mix IP versions in listen address and default target address")
		}

		// Check default target address is within network's subnet.
		if netSubnet != nil && !SubnetContainsIP(netSubnet, defaultTargetAddress) {
			return nil, fmt.Errorf("Default target address is not within the network subnet")
		}
	}

	// Validate port rules.
	validPortProcols := []string{"tcp", "udp"}

	// Used to ensure that each listen port is only used once.
	listenPorts := map[string]map[int64]struct{}{
		"tcp": make(map[int64]struct{}),
		"udp": make(map[int64]struct{}),
	}

	// Maps portSpecID to a portMap struct.
	portSpecsMap := make(map[int]*forwardPortMap)

	for portSpecID, portSpec := range forward.Ports {
		if !shared.StringInSlice(portSpec.Protocol, validPortProcols) {
			return nil, fmt.Errorf("Invalid port protocol in port specification %d, protocol must be one of: %s", portSpecID, strings.Join(validPortProcols, ", "))
		}

		targetAddress := net.ParseIP(portSpec.TargetAddress)
		if targetAddress == nil {
			return nil, fmt.Errorf("Invalid target address in port specification %d", portSpecID)
		}

		if targetAddress.Equal(defaultTargetAddress) {
			return nil, fmt.Errorf("Target address is same as default target address in port specification %d", portSpecID)
		}

		targetIsIP4 := targetAddress.To4() != nil
		if listenIsIP4 != targetIsIP4 {
			return nil, fmt.Errorf("Cannot mix IP versions in listen address and port specification %d target address", portSpecID)
		}

		// Check target address is within network's subnet.
		if netSubnet != nil && !SubnetContainsIP(netSubnet, targetAddress) {
			return nil, fmt.Errorf("Target address is not within the network subnet in port specification %d", portSpecID)
		}

		// Check valid listen port(s) supplied.
		listenPortRanges := shared.SplitNTrimSpace(portSpec.ListenPort, ",", -1, true)
		if len(listenPortRanges) <= 0 {
			return nil, fmt.Errorf("Missing listen port in port specification %d", portSpecID)
		}

		portMap := forwardPortMap{
			listenPorts:   make([]uint64, 0),
			targetAddress: targetAddress,
			protocol:      portSpec.Protocol,
		}

		for _, pr := range listenPortRanges {
			portFirst, portRange, err := ParsePortRange(pr)
			if err != nil {
				return nil, fmt.Errorf("Invalid listen port in port specification %d: %w", portSpecID, err)
			}

			for i := int64(0); i < portRange; i++ {
				port := portFirst + i
				if _, found := listenPorts[portSpec.Protocol][port]; found {
					return nil, fmt.Errorf("Duplicate listen port %d for protocol %q in port specification %d", port, portSpec.Protocol, portSpecID)
				}

				listenPorts[portSpec.Protocol][port] = struct{}{}
				portMap.listenPorts = append(portMap.listenPorts, uint64(port))
			}
		}

		// Check valid target port(s) supplied.
		targetPortRanges := shared.SplitNTrimSpace(portSpec.TargetPort, ",", -1, true)

		if len(targetPortRanges) > 0 {
			// Target ports can be at maximum the same length as listen ports.
			portMap.targetPorts = make([]uint64, 0, len(portMap.listenPorts))

			for _, pr := range targetPortRanges {
				portFirst, portRange, err := ParsePortRange(pr)
				if err != nil {
					return nil, fmt.Errorf("Invalid target port in port specification %d", portSpecID)
				}

				for i := int64(0); i < portRange; i++ {
					port := portFirst + i
					portMap.targetPorts = append(portMap.targetPorts, uint64(port))
				}
			}

			// Only check if the target port count matches the listen port count if the target ports
			// don't equal 1, because we allow many-to-one type mapping.
			portSpectTargetPortsLen := len(portMap.targetPorts)
			if portSpectTargetPortsLen != 1 && len(portMap.listenPorts) != portSpectTargetPortsLen {
				return nil, fmt.Errorf("Mismatch of listen port(s) and target port(s) count in port specification %d", portSpecID)
			}
		}

		portSpecsMap[portSpecID] = &portMap
	}

	portMaps := make([]*forwardPortMap, 0)
	for _, portMap := range portSpecsMap {
		portMaps = append(portMaps, portMap)
	}

	return portMaps, err
}

// ForwardCreate returns ErrNotImplemented for drivers that do not support forwards.
func (n *common) ForwardCreate(forward api.NetworkForwardsPost, clientType request.ClientType) error {
	return ErrNotImplemented
}

// ForwardUpdate returns ErrNotImplemented for drivers that do not support forwards.
func (n *common) ForwardUpdate(listenAddress string, newForward api.NetworkForwardPut, clientType request.ClientType) error {
	return ErrNotImplemented
}

// ForwardDelete returns ErrNotImplemented for drivers that do not support forwards.
func (n *common) ForwardDelete(listenAddress string, clientType request.ClientType) error {
	return ErrNotImplemented
}

// forwardBGPSetupPrefixes exports external forward addresses as prefixes.
func (n *common) forwardBGPSetupPrefixes() error {
	// Retrieve network forwards before clearing existing prefixes, and separate them by IP family.
	fwdListenAddresses, err := n.state.DB.Cluster.GetNetworkForwardListenAddresses(n.ID(), true)
	if err != nil {
		return fmt.Errorf("Failed loading network forwards: %w", err)
	}

	fwdListenAddressesByFamily := map[uint][]string{
		4: make([]string, 0),
		6: make([]string, 0),
	}

	for _, fwdListenAddress := range fwdListenAddresses {
		if strings.Contains(fwdListenAddress, ":") {
			fwdListenAddressesByFamily[6] = append(fwdListenAddressesByFamily[6], fwdListenAddress)
		} else {
			fwdListenAddressesByFamily[4] = append(fwdListenAddressesByFamily[4], fwdListenAddress)
		}
	}

	// Use forward specific owner string (different from the network prefixes) so that these can be reapplied
	// independently of the network's own prefixes.
	bgpOwner := fmt.Sprintf("network_%d_forward", n.id)

	// Clear existing address forward prefixes for network.
	err = n.state.BGP.RemovePrefixByOwner(bgpOwner)
	if err != nil {
		return err
	}

	// Add the new prefixes.
	for _, ipVersion := range []uint{4, 6} {
		nextHopAddr := n.bgpNextHopAddress(ipVersion)
		natEnabled := shared.IsTrue(n.config[fmt.Sprintf("ipv%d.nat", ipVersion)])
		_, netSubnet, _ := net.ParseCIDR(n.config[fmt.Sprintf("ipv%d.address", ipVersion)])

		routeSubnetSize := 128
		if ipVersion == 4 {
			routeSubnetSize = 32
		}

		// Export external forward listen addresses.
		for _, fwdListenAddress := range fwdListenAddressesByFamily[ipVersion] {
			fwdListenAddr := net.ParseIP(fwdListenAddress)

			// Don't export internal address forwards (those inside the NAT enabled network's subnet).
			if natEnabled && netSubnet != nil && netSubnet.Contains(fwdListenAddr) {
				continue
			}

			_, ipRouteSubnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", fwdListenAddr.String(), routeSubnetSize))
			if err != nil {
				return err
			}

			err = n.state.BGP.AddPrefix(*ipRouteSubnet, nextHopAddr, bgpOwner)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Leases returns ErrNotImplemented for drivers that don't support address leases.
func (n *common) Leases(projectName string, clientType request.ClientType) ([]api.NetworkLease, error) {
	return nil, ErrNotImplemented
}

// PeerCrete returns ErrNotImplemented for drivers that do not support forwards.
func (n *common) PeerCreate(forward api.NetworkPeersPost) error {
	return ErrNotImplemented
}

// PeerUpdate returns ErrNotImplemented for drivers that do not support forwards.
func (n *common) PeerUpdate(peerName string, newPeer api.NetworkPeerPut) error {
	return ErrNotImplemented
}

// PeerDelete returns ErrNotImplemented for drivers that do not support forwards.
func (n *common) PeerDelete(peerName string) error {
	return ErrNotImplemented
}

// peerValidate valites the peer request.
func (n *common) peerValidate(peerName string, peer *api.NetworkPeerPut) error {
	err := acl.ValidName(peerName)
	if err != nil {
		return err
	}

	if shared.StringInSlice(peerName, acl.ReservedNetworkSubects) {
		return fmt.Errorf("Name cannot be one of the reserved network subjects: %v", acl.ReservedNetworkSubects)
	}

	// Look for any unknown config fields.
	for k := range peer.Config {
		if k == "target_address" {
			continue
		}

		// User keys are not validated.
		if shared.IsUserConfig(k) {
			continue
		}

		return fmt.Errorf("Invalid option %q", k)
	}

	return nil
}

// PeerUsedBy returns a list of API endpoints referencing this peer.
func (n *common) PeerUsedBy(peerName string) ([]string, error) {
	return n.peerUsedBy(peerName, false)
}

// isUsed returns whether or not the peer is in use.
func (n *common) peerIsUsed(peerName string) (bool, error) {
	usedBy, err := n.peerUsedBy(peerName, true)
	if err != nil {
		return false, err
	}

	return len(usedBy) > 0, nil
}

// peerUsedBy returns a list of API endpoints referencing this peer.
func (n *common) peerUsedBy(peerName string, firstOnly bool) ([]string, error) {
	usedBy := []string{}

	rulesUsePeer := func(rules []api.NetworkACLRule) bool {
		for _, rule := range rules {
			for _, subject := range shared.SplitNTrimSpace(rule.Source, ",", -1, true) {
				if !strings.HasPrefix(subject, "@") {
					continue
				}

				peerParts := strings.SplitN(strings.TrimPrefix(subject, "@"), "/", 2)
				if len(peerParts) != 2 {
					continue // Not a valid network/peer name combination.
				}

				peer := db.NetworkPeer{
					NetworkName: peerParts[0],
					PeerName:    peerParts[1],
				}

				if peer.NetworkName == n.Name() && peer.PeerName == peerName {
					return true
				}
			}
		}

		return false
	}

	// Find ACLs that have rules that reference the peer connection.
	aclNames, err := n.state.DB.Cluster.GetNetworkACLs(n.Project())
	if err != nil {
		return nil, err
	}

	for _, aclName := range aclNames {
		_, aclInfo, err := n.state.DB.Cluster.GetNetworkACL(n.Project(), aclName)
		if err != nil {
			return nil, err
		}

		// Ingress rules can specify peer names in their Source subjects.
		for _, rules := range [][]api.NetworkACLRule{aclInfo.Ingress, aclInfo.Egress} {
			if rulesUsePeer(rules) {
				usedBy = append(usedBy, api.NewURL().Project(n.Project()).Path(version.APIVersion, "network-acls", aclName).String())

				if firstOnly {
					return usedBy, err
				}

				break
			}
		}
	}

	return usedBy, nil
}

func (n *common) State() (*api.NetworkState, error) {
	return resources.GetNetworkState(n.name)
}

func (n *common) setUnavailable() {
	pn := ProjectNetwork{
		ProjectName: n.Project(),
		NetworkName: n.Name(),
	}

	unavailableNetworksMu.Lock()
	unavailableNetworks[pn] = struct{}{}
	unavailableNetworksMu.Unlock()
}

func (n *common) setAvailable() {
	pn := ProjectNetwork{
		ProjectName: n.Project(),
		NetworkName: n.Name(),
	}

	unavailableNetworksMu.Lock()
	delete(unavailableNetworks, pn)
	unavailableNetworksMu.Unlock()
}
