// Command avd is a thin CLI client for the avdd control plane.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `usage: avd <command> [flags]

commands:
  start [--api N] [--gpu host|swiftshader] [--ttl DUR] [--json]
        Start a device; blocks until booted, prints id/adb/screen.
  list  [--json]
        List live devices.
  stop <id>
        Stop and destroy a device.
  adb <id> -- <args...>
        Run the local adb against the device's endpoint.
  screen <id>
        Print (and on a desktop, open) the device's screen URL.

environment:
  AVD_ENDPOINT   Base URL of the avdd control plane (required).
  AVD_USER/AVD_PASS
                 HTTP basic-auth credentials for the fronting proxy.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	c, err := newClient()
	if err != nil {
		fatal(err)
	}
	switch os.Args[1] {
	case "start":
		err = cmdStart(c, os.Args[2:])
	case "list":
		err = cmdList(c, os.Args[2:])
	case "stop":
		err = cmdStop(c, os.Args[2:])
	case "adb":
		err = cmdADB(c, os.Args[2:])
	case "screen":
		err = cmdScreen(c, os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "avd: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "avd:", err)
	os.Exit(1)
}

// client wraps HTTP access to avdd, adding the proxy's basic-auth creds.
type client struct {
	endpoint string
	user     string
	pass     string
	http     *http.Client
}

func newClient() (*client, error) {
	endpoint := strings.TrimRight(os.Getenv("AVD_ENDPOINT"), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("AVD_ENDPOINT is not set; point it at the avdd control plane")
	}
	return &client{
		endpoint: endpoint,
		user:     os.Getenv("AVD_USER"),
		pass:     os.Getenv("AVD_PASS"),
		http:     &http.Client{Timeout: 10 * time.Minute},
	}, nil
}

// do performs a request and decodes the JSON response into out (if non-nil).
func (c *client) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.endpoint+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s (%s)", e.Error, resp.Status)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type device struct {
	ID       string    `json:"id"`
	API      int       `json:"api"`
	GPU      string    `json:"gpu"`
	ADBPort  int       `json:"adbPort"`
	VNCPort  int       `json:"vncPort"`
	Created  time.Time `json:"created"`
	Deadline time.Time `json:"deadline"`
	TTL      string    `json:"ttl"`
	State    string    `json:"state"`
	ADB      string    `json:"adb"`
	Screen   string    `json:"screen"`
}

func cmdStart(c *client, args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	api := fs.Int("api", 34, "Android API level")
	gpu := fs.String("gpu", "", "render mode: host or swiftshader (server default if empty)")
	ttl := fs.String("ttl", "", "session TTL, e.g. 90m (server default if empty)")
	asJSON := fs.Bool("json", false, "print the raw JSON response")
	fs.Parse(args)

	body := map[string]any{"api": *api}
	if *gpu != "" {
		body["gpu"] = *gpu
	}
	if *ttl != "" {
		body["ttl"] = *ttl
	}
	var resp struct {
		ID     string `json:"id"`
		ADB    string `json:"adb"`
		Screen string `json:"screen"`
	}
	if err := c.do(http.MethodPost, "/devices", body, &resp); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	fmt.Printf("id:     %s\nadb:    %s\nscreen: %s\n", resp.ID, resp.ADB, resp.Screen)
	return nil
}

func cmdList(c *client, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the raw JSON response")
	fs.Parse(args)

	var devs []device
	if err := c.do(http.MethodGet, "/devices", nil, &devs); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(devs)
	}
	if len(devs) == 0 {
		fmt.Println("no devices")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tAPI\tSTATE\tADB PORT\tAGE\tEXPIRES IN")
	for _, d := range devs {
		age := "-"
		if !d.Created.IsZero() {
			age = time.Since(d.Created).Round(time.Second).String()
		}
		expires := "-"
		if !d.Deadline.IsZero() {
			expires = time.Until(d.Deadline).Round(time.Second).String()
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\t%s\n", d.ID, d.API, d.State, d.ADBPort, age, expires)
	}
	return w.Flush()
}

func cmdStop(c *client, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: avd stop <id>")
	}
	if err := c.do(http.MethodDelete, "/devices/"+args[0], nil, nil); err != nil {
		return err
	}
	fmt.Printf("stopped %s\n", args[0])
	return nil
}

func cmdADB(c *client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: avd adb <id> -- <args...>")
	}
	id := args[0]
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: avd adb <id> -- <args...>")
	}
	var d device
	if err := c.do(http.MethodGet, "/devices/"+id, nil, &d); err != nil {
		return err
	}
	// A TCP target must be connected before -s can address it; best-effort,
	// since it may already be connected from a previous invocation.
	exec.Command("adb", "connect", d.ADB).Run()
	cmd := exec.Command("adb", append([]string{"-s", d.ADB}, rest...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func cmdScreen(c *client, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: avd screen <id>")
	}
	var d device
	if err := c.do(http.MethodGet, "/devices/"+args[0], nil, &d); err != nil {
		return err
	}
	fmt.Println(d.Screen)
	openBrowser(d.Screen)
	return nil
}

// openBrowser best-effort opens the URL on a desktop; failures are silent
// because printing the URL is the contract.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	cmd.Start()
}
