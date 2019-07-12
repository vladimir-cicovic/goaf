package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Facts holds system information gathered from a remote host.
type Facts map[string]string

// GatherFactsAll collects facts from all hosts in parallel.
// Hosts that fail fact gathering receive an empty Facts map so tasks can still run.
func GatherFactsAll(hosts []Host, parallelism int) map[string]Facts {
	if parallelism < 1 {
		parallelism = 1
	}
	result := make(map[string]Facts, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelism)

	for _, host := range hosts {
		wg.Add(1)
		go func(h Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			label := hostLabel(h)
			sess, err := Connect(h)
			if err != nil {
				mu.Lock()
				result[label] = Facts{}
				mu.Unlock()
				return
			}
			defer sess.Close()

			mu.Lock()
			result[label] = gatherOne(sess)
			mu.Unlock()
		}(host)
	}
	wg.Wait()
	return result
}

func gatherOne(sess *Session) Facts {
	facts := Facts{}

	// Hostname via /proc — no hostname binary required.
	if out, err := sess.Run("cat /proc/sys/kernel/hostname 2>/dev/null"); err == nil {
		facts["goaf_hostname"] = strings.TrimSpace(out)
	}

	if out, err := sess.Run("uname -m 2>/dev/null"); err == nil {
		facts["goaf_arch"] = strings.TrimSpace(out)
	}

	if out, err := sess.Run("uname -r 2>/dev/null"); err == nil {
		facts["goaf_kernel"] = strings.TrimSpace(out)
	}

	// Primary IP: try ip route, fall back to ip addr.
	ipCmd := "ip route get 1.1.1.1 2>/dev/null | awk 'NR==1{for(i=1;i<=NF;i++) if($i==\"src\") print $(i+1)}'"
	if out, err := sess.Run(ipCmd); err == nil {
		if ip := strings.TrimSpace(out); ip != "" {
			facts["goaf_ip"] = ip
		}
	}
	if _, ok := facts["goaf_ip"]; !ok {
		fallback := "ip -4 addr show scope global 2>/dev/null | awk '/inet/{gsub(/\\/[0-9]+/,\"\",$2); print $2; exit}'"
		if out, err := sess.Run(fallback); err == nil {
			if ip := strings.TrimSpace(out); ip != "" {
				facts["goaf_ip"] = ip
			}
		}
	}

	// OS identification from /etc/os-release.
	if out, err := sess.Run("cat /etc/os-release 2>/dev/null"); err == nil {
		parseOSRelease(out, facts)
	}

	return facts
}

func parseOSRelease(content string, facts Facts) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.Trim(strings.TrimSpace(line[idx+1:]), `"'`)
		switch key {
		case "ID":
			facts["goaf_os"] = strings.ToLower(val)
		case "NAME":
			facts["goaf_os_name"] = val
		case "VERSION_ID":
			facts["goaf_os_version"] = val
		case "ID_LIKE":
			// Normalise the first word through deriveFamily so all redhat-family
			// distros map to "redhat" regardless of their specific ID_LIKE value.
			fields := strings.Fields(strings.ToLower(val))
			if len(fields) > 0 {
				facts["goaf_os_family"] = deriveFamily(fields[0])
			}
		}
	}
	// Derive family from ID when ID_LIKE is absent.
	if _, ok := facts["goaf_os_family"]; !ok {
		if os, ok := facts["goaf_os"]; ok {
			facts["goaf_os_family"] = deriveFamily(os)
		}
	}
}

func deriveFamily(os string) string {
	switch os {
	case "debian", "ubuntu", "linuxmint", "raspbian", "kali":
		return "debian"
	case "fedora", "rhel", "centos", "almalinux", "rocky", "ol", "amzn":
		return "redhat"
	case "alpine":
		return "alpine"
	case "arch", "manjaro", "artix", "endeavouros":
		return "arch"
	case "opensuse-leap", "opensuse-tumbleweed", "opensuse", "suse", "sles":
		return "suse"
	case "gentoo":
		return "gentoo"
	case "slackware":
		return "slackware"
	}
	return os
}

// ---------- setup module ----------
// Gathers and displays host facts. Always runs (Check=true), shows facts as output.

type SetupModule struct{}

func (m SetupModule) Name() string                   { return "setup" }
func (m SetupModule) Check(_ *Session) (bool, error) { return true, nil }

func (m SetupModule) Apply(s *Session) (string, error) {
	facts := gatherOne(s)
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%-20s = %s", k, facts[k]))
	}
	return strings.Join(lines, "\n"), nil
}
