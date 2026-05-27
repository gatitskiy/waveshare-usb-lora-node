package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/Archie3d/waveshare-usb-lora-client/pkg/meshtastic"
	"github.com/Archie3d/waveshare-usb-lora-client/pkg/types"
	"github.com/charmbracelet/log"
	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"
)

func usage() {
	flag.PrintDefaults()
}

func showUsageAndExit(exitCode int) {
	fmt.Println("Waveshare Meshtastic Node ntfy.sh message forwarder")
	usage()
	os.Exit(exitCode)
}

type Configuration struct {
	NatsUrl           string `yaml:"nats_url"`
	NatsSubjectPrefix string `yaml:"nats_subject_prefix"`
	NtfyUrl           string `yaml:"ntfy_url"`
	NtfyRecvSubject   string `yaml:"ntfy_recv_subject"`
	NtfySendSubject   string `yaml:"ntfy_send_subject"`
}

func loadConfiguration(configFile string) (*Configuration, error) {
	f, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	config := &Configuration{}
	decoder := yaml.NewDecoder(f)
	err = decoder.Decode(config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

type nodeDirectory struct {
	mu    sync.RWMutex
	names map[types.NodeId]string
}

func newNodeDirectory() *nodeDirectory {
	return &nodeDirectory{names: map[types.NodeId]string{}}
}

func (d *nodeDirectory) set(id types.NodeId, name string) {
	if name == "" {
		return
	}
	d.mu.Lock()
	d.names[id] = name
	d.mu.Unlock()
}

func (d *nodeDirectory) titleFor(id types.NodeId) string {
	d.mu.RLock()
	name, ok := d.names[id]
	d.mu.RUnlock()
	if ok {
		return fmt.Sprintf("Message from %s", name)
	}
	return fmt.Sprintf("Message from node %s", id)
}

func main() {
	var configFile = flag.String("c", "", "Configuration file")
	var logLevel = flag.String("l", "info", "Log level")
	var showHelp = flag.Bool("h", false, "Show help")

	flag.Usage = usage
	flag.Parse()

	if *showHelp {
		showUsageAndExit(0)
	}

	switch *logLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.Fatalf("Invalid log level '%s'", *logLevel)
	}

	if *configFile == "" {
		log.Fatal("Configuration file is not specified")
	}

	config, err := loadConfiguration(*configFile)
	if err != nil {
		log.With("err", err).Fatal("Failed to load configuration")
	}

	if config.NtfyUrl == "" {
		config.NtfyUrl = "https://ntfy.sh"
	}
	config.NtfyUrl = strings.TrimRight(config.NtfyUrl, "/")

	nc, err := nats.Connect(config.NatsUrl)
	if err != nil {
		log.With("err", err).Fatal("Failed to connect to NATS server")
	}

	defer nc.Close()

	directory := newNodeDirectory()

	nodeInfoSub, err := nc.Subscribe(config.NatsSubjectPrefix+".in.node_info", func(msg *nats.Msg) {
		var info meshtastic.NodeInfoApplicationIncomingMessage
		if err := json.Unmarshal(msg.Data, &info); err != nil {
			log.With("err", err).Error("Failed to unmarshal node info")
			return
		}
		name := info.LongName
		if name == "" {
			name = info.ShortName
		}
		directory.set(info.From, name)
		log.With("from", info.From, "long_name", info.LongName, "short_name", info.ShortName).Debug("Learned node name")
	})
	if err != nil {
		log.With("err", err).Fatal("Failed to subscribe to node_info")
	}

	sub, err := nc.Subscribe(config.NatsSubjectPrefix+".in.text", func(msg *nats.Msg) {
		var message meshtastic.TextApplicationIncomingMessage
		err := json.Unmarshal(msg.Data, &message)
		if err != nil {
			log.With("err", err).Error("Failed to unmarshal message")
			return
		}

		title := directory.titleFor(message.From)
		log.With("from", message.From, "title", title, "text", message.Text).Info("Forwarding message")

		req, err := http.NewRequest("POST", config.NtfyUrl+"/"+config.NtfyRecvSubject, bytes.NewBufferString(message.Text))
		if err != nil {
			log.With("err", err).Error("Failed to create request to ntfy")
			return
		}
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Title", title)
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			log.With("err", err).Error("Failed to forward message to ntfy")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.With("status", resp.Status, "url", req.URL.String()).Error("ntfy rejected the message")
		}
	})
	if err != nil {
		log.With("err", err).Fatal("Failed to subscribe to text")
	}

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		sub.Unsubscribe()
		nodeInfoSub.Unsubscribe()
		os.Exit(0)
	}()

	log.Info("Ntfy service is up an running")

	select {}
}
