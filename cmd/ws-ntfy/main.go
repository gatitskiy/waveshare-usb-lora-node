package main

import (
	"bufio"
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
	"time"

	"github.com/Archie3d/waveshare-usb-lora-client/pkg/meshtastic"
	"github.com/Archie3d/waveshare-usb-lora-client/pkg/types"
	"github.com/charmbracelet/log"
	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"
)

const wsNtfyTag = "ws-ntfy-bridge"

func usage() {
	flag.PrintDefaults()
}

func showUsageAndExit(exitCode int) {
	fmt.Println("Waveshare Meshtastic Node ntfy.sh message forwarder")
	usage()
	os.Exit(exitCode)
}

type ChannelMapping struct {
	Id    uint32 `yaml:"id"`
	Topic string `yaml:"topic"`
}

type Configuration struct {
	NatsUrl           string           `yaml:"nats_url"`
	NatsSubjectPrefix string           `yaml:"nats_subject_prefix"`
	NtfyUrl           string           `yaml:"ntfy_url"`
	Channels          []ChannelMapping `yaml:"channels"`
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

type ntfyEvent struct {
	Event   string   `json:"event"`
	Topic   string   `json:"topic"`
	Message string   `json:"message"`
	Tags    []string `json:"tags"`
}

func (e *ntfyEvent) hasTag(tag string) bool {
	for _, t := range e.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

func runReplyBridge(ntfyUrl, topic string, channelId uint32, outSubject string, nc *nats.Conn) {
	url := fmt.Sprintf("%s/%s/json", ntfyUrl, topic)
	backoff := time.Second

	for {
		err := streamReplies(url, topic, channelId, outSubject, nc)
		if err != nil {
			log.With("err", err, "topic", topic).Warn("ntfy reply stream broke, reconnecting")
		} else {
			log.With("topic", topic).Warn("ntfy reply stream closed cleanly, reconnecting")
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func streamReplies(url, topic string, channelId uint32, outSubject string, nc *nats.Conn) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{} // no timeout: keep the long-poll open
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("ntfy returned %s", resp.Status)
	}

	log.With("topic", topic, "channel", channelId).Info("Listening on ntfy for replies")

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev ntfyEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			log.With("err", err, "raw", string(line)).Debug("Skipping non-JSON line")
			continue
		}
		if ev.Event != "message" {
			continue
		}
		if ev.hasTag(wsNtfyTag) {
			continue
		}
		if ev.Message == "" {
			continue
		}

		out := map[string]interface{}{
			"channel": channelId,
			"to":      "ffffffff",
			"text":    ev.Message,
		}
		data, err := json.Marshal(out)
		if err != nil {
			log.With("err", err).Error("Failed to marshal outgoing message")
			continue
		}
		if err := nc.Publish(outSubject, data); err != nil {
			log.With("err", err).Error("Failed to publish reply to NATS")
			continue
		}
		log.With("text", ev.Message, "channel", channelId, "topic", topic).Info("Forwarded ntfy reply to mesh")
	}
	return scanner.Err()
}

func postIncoming(ntfyUrl, topic, title, text string) error {
	req, err := http.NewRequest("POST", ntfyUrl+"/"+topic, bytes.NewBufferString(text))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Title", title)
	req.Header.Set("Tags", wsNtfyTag)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ntfy returned %s", resp.Status)
	}
	return nil
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

	if len(config.Channels) == 0 {
		log.Fatal("No channels configured (set 'channels:' in ntfy.yaml)")
	}

	topicByChannel := map[uint32]string{}
	for _, ch := range config.Channels {
		if ch.Topic == "" {
			log.With("channel", ch.Id).Fatal("Channel mapping has empty topic")
		}
		if existing, ok := topicByChannel[ch.Id]; ok {
			log.With("channel", ch.Id, "topics", []string{existing, ch.Topic}).Fatal("Duplicate channel id in channels mapping")
		}
		topicByChannel[ch.Id] = ch.Topic
	}

	nc, err := nats.Connect(config.NatsUrl)
	if err != nil {
		log.With("err", err).Fatal("Failed to connect to NATS server")
	}

	defer nc.Close()

	directory := newNodeDirectory()
	outSubject := config.NatsSubjectPrefix + ".out.text"

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

		topic, ok := topicByChannel[message.ChannelId]
		if !ok {
			log.With("from", message.From, "channel", message.ChannelId).
				Debug("No ntfy topic mapped for this channel; dropping")
			return
		}

		title := directory.titleFor(message.From)
		log.With("from", message.From, "channel", message.ChannelId, "topic", topic, "text", message.Text).
			Info("Forwarding message")

		if err := postIncoming(config.NtfyUrl, topic, title, message.Text); err != nil {
			log.With("err", err, "topic", topic).Error("Failed to forward message to ntfy")
		}
	})
	if err != nil {
		log.With("err", err).Fatal("Failed to subscribe to text")
	}

	for _, ch := range config.Channels {
		go runReplyBridge(config.NtfyUrl, ch.Topic, ch.Id, outSubject, nc)
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
