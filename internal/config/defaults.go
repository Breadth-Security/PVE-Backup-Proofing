package config

// DefaultIgnoreUnits are systemd units ignored when judging failed units: each
// fails in the proof network because it has no uplink.
func DefaultIgnoreUnits() []string {
	return []string{
		"systemd-networkd-wait-online.service", "NetworkManager-wait-online.service",
		"chronyd.service", "chrony.service", "systemd-timesyncd.service",
		"ntp.service", "ntpd.service", "ntpsec.service",
		"remote-fs.target", "nfs-client.target", "rpc-statd.service",
		"cloud-init.service", "cloud-init-network.service",
		"cloud-final.service", "cloud-config.service",
	}
}

// DefaultWindowsIgnore are Windows services that routinely stop or fail in an
// isolated, uplink-less guest (activation, updates, time sync, delayed-start
// brokers).
func DefaultWindowsIgnore() []string {
	return []string{
		"sppsvc", "gupdate", "edgeupdate", "MapsBroker", "RemoteRegistry",
		"CDPSvc", "tiledatamodelsvc", "wuauserv", "BITS", "W32Time",
	}
}

// BuiltinProfiles are the service profiles applied when a matching unit is
// present in the guest. Profile ports pass if any listed port listens.
func BuiltinProfiles() []Profile {
	web := []int{80, 443}
	return []Profile{
		{Name: "postgresql", Unit: "postgresql*.service", CheckSet: CheckSet{Commands: []string{"pg_isready -q"}}},
		{Name: "mariadb", Unit: "mariadb.service", CheckSet: CheckSet{Commands: []string{"mariadb-admin ping"}}},
		{Name: "mysql", Unit: "mysql*.service", CheckSet: CheckSet{Commands: []string{"mysqladmin ping"}}},
		{Name: "nginx", Unit: "nginx.service", CheckSet: CheckSet{Ports: web}},
		{Name: "apache", Unit: "apache2.service", CheckSet: CheckSet{Ports: web}},
		{Name: "httpd", Unit: "httpd.service", CheckSet: CheckSet{Ports: web}},
		{Name: "docker", Unit: "docker.service", CheckSet: CheckSet{Commands: []string{"docker info"}}},
		{Name: "redis", Unit: "redis*.service", CheckSet: CheckSet{Commands: []string{"redis-cli ping"}}},
		{Name: "ssh", Unit: "ssh.service", CheckSet: CheckSet{Ports: []int{22}}},
		{Name: "sshd", Unit: "sshd.service", CheckSet: CheckSet{Ports: []int{22}}},
		{Name: "cron", Unit: "cron.service"},
		{Name: "crond", Unit: "crond.service"},
	}
}
