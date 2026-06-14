package install

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const (
	serviceName = "mirror-relay"
	serviceUser = "mirror-relay"
	binPath     = "/usr/local/bin/relay"
	configDir   = "/etc/mirror-relay"
	configFile  = "/etc/mirror-relay/config.yaml"
	cacheDir    = "/var/cache/mirror-relay"
	unitFile    = "/etc/systemd/system/mirror-relay.service"
)

const defaultConfig = `relay:
  # secret_key: ""  # shared across all relay levels; auto-generated if omitted
  # disguise_port: 344  # SSH-disguised TLS port (bypasses HTTPS throttling)
  base_domains:
    - domain: relay.example.com
      cf_api_token: ""  # CF API token for DNS-01 (each domain has its own)

upstream:
  url: "https://mirs.uk"
  # disguise_port: 344  # connect to upstream relay via SSH-disguised TLS

acme:
  source: acme  # "acme" = DNS-01; "upstream" = sync from upstream.url; or a URL origin
`

// PLACEHOLDER_UNIT_TEMPLATE

const unitTemplate = `[Unit]
Description=mirror-db relay - Mirror relay proxy
After=network.target

[Service]
Type=simple
User=%s
Group=%s
ExecStart=%s serve
WorkingDirectory=%s
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

# Allow binding to privileged ports (80/443)
AmbientCapabilities=CAP_NET_BIND_SERVICE

# Security
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=%s %s

[Install]
WantedBy=multi-user.target
`

// Cmd returns the cobra command for install.
func Cmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install relay as a systemd service",
		Run: func(cmd *cobra.Command, args []string) {
			run()
		},
	}
}

func run() {
	if os.Geteuid() != 0 {
		log.Fatal("install must be run as root (use sudo)")
	}

	if isInstalled() {
		runUpdate()
	} else {
		runFreshInstall()
	}
}

func isInstalled() bool {
	_, err := os.Stat(unitFile)
	return err == nil
}

// --- Fresh install ---

func runFreshInstall() {
	fmt.Println("=== mirror-relay install ===")
	fmt.Println()

	ensureUser()
	copyBinary()
	createDirs()
	createConfig()
	writeUnit()

	fmt.Println()
	fmt.Printf("Opening config file for editing: %s\n", configFile)
	fmt.Println("Please configure your relay domains and tokens.")
	fmt.Println()
	openEditor(configFile)

	fmt.Println()
	if confirm("Enable and start the service now?") {
		systemctl("daemon-reload")
		systemctl("enable", serviceName)
		systemctl("start", serviceName)
		fmt.Println()
		fmt.Println("Service started. Check status:")
		fmt.Printf("  sudo systemctl status %s\n", serviceName)
		fmt.Printf("  sudo journalctl -u %s -f\n", serviceName)
	} else {
		systemctl("daemon-reload")
		fmt.Println()
		fmt.Println("Service installed but not started. To start later:")
		fmt.Printf("  sudo systemctl enable --now %s\n", serviceName)
	}
}

// --- Update ---

func runUpdate() {
	fmt.Println("=== mirror-relay update ===")
	fmt.Printf("Existing installation detected at %s\n", unitFile)
	fmt.Println()

	if confirm("Update binary?") {
		copyBinary()
		writeUnit() // refresh unit in case template changed
		systemctl("daemon-reload")

		fmt.Println()
		if confirm("Restart service now?") {
			systemctl("restart", serviceName)
			fmt.Println("Service restarted.")
		} else {
			fmt.Println("Binary updated. Restart manually when ready:")
			fmt.Printf("  sudo systemctl restart %s\n", serviceName)
		}
	} else {
		fmt.Println("No changes made.")
	}
}

// --- Helpers ---

func ensureUser() {
	// Check if user already exists
	if err := exec.Command("id", serviceUser).Run(); err == nil {
		fmt.Printf("User %s already exists\n", serviceUser)
		return
	}

	// User might not exist but group might (leftover from previous uninstall).
	// Use -g to join existing group, or let useradd create both.
	args := []string{"-r", "-s", "/bin/false", "-d", configDir}
	if exec.Command("getent", "group", serviceUser).Run() == nil {
		args = append(args, "-g", serviceUser)
	}
	args = append(args, serviceUser)

	cmd := exec.Command("useradd", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("failed to create user %s: %v", serviceUser, err)
	}
	fmt.Printf("Created system user: %s\n", serviceUser)
}

func copyBinary() {
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("failed to get executable path: %v", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		log.Fatalf("failed to resolve executable path: %v", err)
	}

	if self == binPath {
		fmt.Printf("Binary already at %s\n", binPath)
		return
	}

	data, err := os.ReadFile(self)
	if err != nil {
		log.Fatalf("failed to read self: %v", err)
	}

	// Write to temp file then atomic rename — avoids "text file busy" when
	// the service is running (old process keeps the old inode via open fd).
	tmp := binPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		log.Fatalf("failed to write temp binary: %v", err)
	}
	if err := os.Rename(tmp, binPath); err != nil {
		os.Remove(tmp)
		log.Fatalf("failed to install binary to %s: %v", binPath, err)
	}
	fmt.Printf("Installed binary to %s\n", binPath)
}

func createDirs() {
	for _, dir := range []struct {
		path string
		perm os.FileMode
	}{
		{configDir, 0755},
		{cacheDir, 0755},
	} {
		if err := os.MkdirAll(dir.path, dir.perm); err != nil {
			log.Fatalf("failed to create %s: %v", dir.path, err)
		}
	}
	// Ensure ownership
	chown(configDir, serviceUser)
	chown(cacheDir, serviceUser)
}

func createConfig() {
	if _, err := os.Stat(configFile); err == nil {
		fmt.Printf("Config file already exists: %s\n", configFile)
		return
	}

	if err := os.WriteFile(configFile, []byte(defaultConfig), 0644); err != nil {
		log.Fatalf("failed to write config: %v", err)
	}
	fmt.Printf("Created config: %s\n", configFile)
}

func writeUnit() {
	content := fmt.Sprintf(unitTemplate,
		serviceUser, serviceUser, binPath, configDir, configDir, cacheDir)
	if err := os.WriteFile(unitFile, []byte(content), 0644); err != nil {
		log.Fatalf("failed to write unit file: %v", err)
	}
	fmt.Printf("Written systemd unit: %s\n", unitFile)
}

func openEditor(file string) {
	editor := findEditor()
	cmd := exec.Command(editor, file)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("editor exited with error: %v", err)
	}
}

func findEditor() string {
	for _, env := range []string{"EDITOR", "VISUAL"} {
		if e := os.Getenv(env); e != "" {
			return e
		}
	}
	for _, name := range []string{"vim", "nano", "vi"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return "vi"
}

func confirm(prompt string) bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("%s [Y/n] ", prompt)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || answer == "y" || answer == "yes"
}

func chown(path, user string) {
	cmd := exec.Command("chown", "-R", user+":"+user, path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("chown %s: %v", path, err)
	}
}

func systemctl(args ...string) {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("systemctl %s: %v", strings.Join(args, " "), err)
	}
}
