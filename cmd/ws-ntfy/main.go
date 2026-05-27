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
	"sync/atomic"
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

type Configuration struct {
	NatsUrl                string `yaml:"nats_url"`
	NatsSubjectPrefix      string `yaml:"nats_subject_prefix"`
	NtfyUrl                string `yaml:"ntfy_url"`
	NtfyRecvSubject        string `yaml:"ntfy_recv_subject"`
	NtfySendSubject        string `yaml:"ntfy_send_subject"`
	NtfyDefaultSendChannel uint32 `yaml:"ntfy_default_send_channel"`
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

// replyState tracks the channel of the most recently received text message so
// outgoing replies from ntfy land in the same channel (DMs aren't supported —
// we always broadcast).
type replyState struct {
	lastChannel       atomic.Uint32
	hasSeenIncoming   atomic.Bool
	defaultChannel    uint32
}

func newReplyState(defaultChannel uint32) *replyState {
	return &replyState{defaultChannel: defaultChannel}
}

func (s *replyState) record(channel uint32) {
	s.lastChannel.Store(channel)
	s.hasSeenIncoming.Store(true)
}

func (s *replyState) channelForReply() (uint32, bool) {
	if s.hasSeenIncoming.Load() {
		return s.lastChannel.Load(), true
	}
	return s.defaultChannel, false
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

func runReplyBridge(config *Configuration, nc *nats.Conn, state *replyState) {
	url := fmt.Sprintf("%s/%s/json", config.NtfyUrl, config.NtfySendSubject)
	outSubject := config.NatsSubjectPrefix + ".out.text"
	backoff := time.Second

	for {
		err := streamReplies(url, outSubject, config, nc, state)
		if err != nil {
			log.With("err", err).Warn("ntfy reply stream broke, reconnecting")
		} else {
			log.Warn("ntfy reply stream closed cleanly, reconnecting")
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func streamReplies(url, outSubject string, config *Configuration, nc *nats.Conn, state *replyState) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{} // no timeout: keep connection open
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("ntfy returned %s", resp.Status)
	}

	log.With("topic", config.NtfySendSubject).Info("Listening on ntfy for replies")

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

		channel, seen := state.channelForReply()
		if !seen {
			log.With("channel", channel).Warn("No incoming text seen yet; using default channel for reply")
		}

		out := map[string]interface{}{
			"channel": channel,
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
		log.With("text", ev.Message, "channel", channel).Info("Forwarded ntfy reply to mesh")
	}
	return scanner.Err()
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
	replies := newReplyState(config.NtfyDefaultSendChannel)

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

		replies.record(message.ChannelId)
		title := directory.titleFor(message.From)
		log.With("from", message.From, "channel", message.ChannelId, "title", title, "text", message.Text).Info("Forwarding message")

		req, err := http.NewRequest("POST", config.NtfyUrl+"/"+config.NtfyRecvSubject, bytes.NewBufferString(message.Text))
		if err != nil {
			log.With("err", err).Error("Failed to create request to ntfy")
			return
		}
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Title", title)
		req.Header.Set("Tags", wsNtfyTag)
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

	if config.NtfySendSubject != "" {
		go runReplyBridge(config, nc, replies)
	} else {
		log.Warn("ntfy_send_subject not set; ntfy → mesh bridge disabled")
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
