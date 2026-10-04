// Command stackchan-usb sets up a Stackchan over its USB cable: it reads the robot id and
// writes a server, its token, autostart and Wi-Fi into the robot's settings (firmware with
// Embody Mode's USB setup; protocol in its usb_setup.h). The same thing chan.w42.eu/setup
// does in Chrome, for developers, own servers and custom firmware.
//
//	stackchan-usb hello
//	stackchan-usb provision -url wss://chan.w42.eu -name chan.w42.eu -token-file token.txt -default -autostart
//	stackchan-usb provision -wifi-ssid Home -wifi-password-file wifi.txt
//	stackchan-usb restart
//	stackchan-usb pair                   the pairing link the robot shows (to open in a browser)
//
// With firmware built with automation, a program can also do what a person at the robot does
// (but never answer the robot's own questions, e.g. a new default server or turning the head):
//
//	stackchan-usb screenshot -o screen.jpg   the screen as a JPEG
//	stackchan-usb tap -x 160 -y 200 [-ms 800]  a tap (or a long press) on the screen
//	stackchan-usb launch -app "Embody Mode"  restart into a launcher app ("launcher": none)
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const prefix = "@stackchan "

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	op, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(op, flag.ExitOnError)
	port := fs.String("port", "", "serial port (default: the first Espressif USB device)")
	var req map[string]any
	out := ""
	switch op {
	case "screenshot":
		o := fs.String("o", "screen.jpg", "where to save the JPEG")
		fs.Parse(args)
		req, out = map[string]any{"op": op}, *o
	case "tap":
		x := fs.Int("x", -1, "x, 0..319 (left to right)")
		y := fs.Int("y", -1, "y, 0..239 (top to bottom)")
		ms := fs.Int("ms", 100, "how long to press, ms (a long press: 800)")
		fs.Parse(args)
		req = map[string]any{"op": op, "x": *x, "y": *y, "ms": *ms}
	case "launch":
		app := fs.String("app", "Embody Mode", `a launcher app's name ("AVATAR", "Embody Mode", ...) or "launcher"`)
		fs.Parse(args)
		req = map[string]any{"op": op, "app": *app}
	case "hello", "restart", "pair":
		fs.Parse(args)
		req = map[string]any{"op": op}
	case "provision":
		url := fs.String("url", "", "server URL, ws:// or wss://, e.g. wss://chan.w42.eu")
		name := fs.String("name", "", "the server's name on the robot (default: the URL)")
		tokenFile := fs.String("token-file", "", "file with the robot's token for that server")
		makeDefault := fs.Bool("default", false, "make it the server the robot connects to at start")
		autostart := fs.Bool("autostart", false, "open Embody Mode after every power-on (firmware with automation)")
		ssid := fs.String("wifi-ssid", "", "Wi-Fi network to add")
		wifiPassFile := fs.String("wifi-password-file", "", "file with the Wi-Fi password")
		fs.Parse(args)
		req = map[string]any{"op": "provision"}
		if *url != "" {
			token, err := readFile(*tokenFile)
			if err != nil {
				fail("token: %v", err)
			}
			req["server"] = map[string]string{"name": *name, "url": *url, "token": token}
			req["default"] = *makeDefault
		}
		if *autostart {
			req["autostart"] = true
		}
		if *ssid != "" {
			pass, err := readFile(*wifiPassFile)
			if err != nil && *wifiPassFile != "" {
				fail("wifi password: %v", err)
			}
			req["wifi"] = map[string]string{"ssid": *ssid, "password": pass}
		}
		if len(req) == 1 {
			fail("nothing to provision: give -url and -token-file, or -wifi-ssid")
		}
	default:
		usage()
	}

	name := *port
	if name == "" {
		var err error
		if name, err = findPort(); err != nil {
			fail("%v", err)
		}
	}
	res, err := talk(name, req)
	if err != nil {
		fail("%v", err)
	}
	if b64, ok := res["jpeg"].(string); ok && out != "" {
		jpeg, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			fail("screenshot: %v", err)
		}
		if err := os.WriteFile(out, jpeg, 0o644); err != nil {
			fail("%v", err)
		}
		delete(res, "jpeg")
		res["saved"] = fmt.Sprintf("%s (%d bytes)", out, len(jpeg))
	}
	pretty, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(pretty))
	if ok, _ := res["ok"].(bool); !ok {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: stackchan-usb hello | provision [flags] | restart | pair | screenshot | tap | launch   (-h for flags)")
	os.Exit(2)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "stackchan-usb: "+format+"\n", a...)
	os.Exit(1)
}

func readFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("no file given")
	}
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// findPort returns the first Espressif USB serial device (USB vendor 0x303A).
func findPort() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", err
	}
	for _, p := range ports {
		if p.IsUSB && strings.EqualFold(p.VID, "303A") {
			return p.Name, nil
		}
	}
	return "", errors.New("no Stackchan on USB: plug it in with a data cable (USB-C on the head)")
}

// talk sends one request and waits for its answer. Opening the port can restart the robot, so
// hello is repeated until the robot answers (it boots in a few seconds); then the request goes
// once: a provision that changes the default server waits for a tap on the robot's screen, and
// repeated copies would pile up in its USB buffer meanwhile.
func talk(name string, req map[string]any) (map[string]any, error) {
	p, err := serial.Open(name, &serial.Mode{BaudRate: 115200})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer p.Close()
	p.SetReadTimeout(500 * time.Millisecond)
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(p)
		sc.Buffer(make([]byte, 64<<10), 1<<20) // a screenshot is one ~40 KB line
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	send := func(r map[string]any) {
		line, _ := json.Marshal(r)
		p.Write([]byte(prefix + string(line) + "\n"))
	}
	answer := func(timeout time.Duration, resend func()) (map[string]any, error) {
		deadline := time.After(timeout)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					return nil, errors.New("the robot went away")
				}
				i := strings.Index(l, prefix)
				if i < 0 {
					continue // a log line
				}
				var res map[string]any
				if json.Unmarshal([]byte(l[i+len(prefix):]), &res) == nil {
					return res, nil
				}
			case <-tick.C:
				if resend != nil {
					resend()
				}
			case <-deadline:
				return nil, errors.New("no answer: is the robot on, with firmware that has USB setup (Embody Mode 2026-10 or newer)?")
			}
		}
	}
	hello := map[string]any{"op": "hello"}
	send(hello)
	first, err := answer(25*time.Second, func() { send(hello) })
	if err != nil || req["op"] == "hello" {
		return first, err
	}
	send(req)
	if req["op"] == "provision" {
		fmt.Fprintln(os.Stderr, "stackchan-usb: if the robot asks on its screen, tap Yes to accept or No to refuse (within a minute)")
	}
	return answer(75*time.Second, nil)
}
