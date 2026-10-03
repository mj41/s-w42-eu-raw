// Command stackchan-usb sets up a Stackchan over its USB cable: it reads the robot id and
// writes a server, its token, autostart and Wi-Fi into the robot's settings (firmware with
// Embody Mode's USB setup; protocol in its usb_setup.h). The same thing chan.w42.eu/setup
// does in Chrome, for developers, own servers and custom firmware.
//
//	stackchan-usb hello
//	stackchan-usb provision -url wss://chan.w42.eu -name chan.w42.eu -token-file token.txt -default -autostart
//	stackchan-usb provision -wifi-ssid Home -wifi-password-file wifi.txt
//	stackchan-usb restart
package main

import (
	"bufio"
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
	switch op {
	case "hello", "restart":
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
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	if ok, _ := res["ok"].(bool); !ok {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: stackchan-usb hello | provision [flags] | restart   (-h for flags)")
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

// talk sends one request and waits for its answer. Opening the port can restart the robot,
// so the request is repeated until the robot answers (it boots in a few seconds).
func talk(name string, req map[string]any) (map[string]any, error) {
	p, err := serial.Open(name, &serial.Mode{BaudRate: 115200})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer p.Close()
	p.SetReadTimeout(500 * time.Millisecond)
	line, _ := json.Marshal(req)
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(p)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(25 * time.Second)
	resend := time.NewTicker(2 * time.Second)
	defer resend.Stop()
	send := func() { p.Write([]byte(prefix + string(line) + "\n")) }
	send()
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
		case <-resend.C:
			if req["op"] != "restart" { // provision and hello are safe to repeat
				send()
			}
		case <-deadline:
			return nil, errors.New("no answer: is the robot on, with firmware that has USB setup (Embody Mode 2026-10 or newer)?")
		}
	}
}
