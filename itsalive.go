package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/nlopes/slack"
)

type duration struct {
	time.Duration
}

type checkStatus int

const (
	checkStatusUnknown checkStatus = iota
	checkStatusOk                  = iota
	checkStatusAlarm               = iota
)

type urlConfig struct {
	Name          string
	URL           string
	OKStatuses    []int
	CheckInterval duration
	OKPeriods     int
	AlarmPeriods  int
	HTTPTimeout   duration
}

type aliveConfig struct {
	Items        []urlConfig
	SlackToken   string
	SlackChannel string
	BotName      string
}

type statusChange struct {
	name string
	url  string
	time time.Time
	from checkStatus
	to   checkStatus
}

func exit() {
	if r := recover(); r != nil {
		log.Fatalf("error: %+v\n", r)
	}
	os.Exit(-1)
}

func (d *duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

func ignoreRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

func checkResponse(resp *http.Response, err error, config urlConfig) bool {
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return intInSlice(resp.StatusCode, config.OKStatuses)
}

func max(x, y int) int {
	if x > y {
		return x
	}
	return y
}

func intInSlice(a int, slice []int) bool {
	for _, b := range slice {
		if a == b {
			return true
		}
	}
	return false
}

func getNewStatus(history []checkStatus, okPeriods int, alarmPeriods int) checkStatus {
	size := len(history)
	if history[size-1] == checkStatusOk {
		var ok = true
		for idx := size - 2; idx >= size-okPeriods; idx-- {
			if history[idx] != checkStatusOk {
				ok = false
				break
			}
		}
		if ok {
			return checkStatusOk
		}
	}
	if history[size-1] == checkStatusAlarm {
		var alarm = true
		for idx := size - 2; idx >= size-alarmPeriods; idx-- {
			if history[idx] != checkStatusAlarm {
				alarm = false
				break
			}
		}
		if alarm {
			return checkStatusAlarm
		}
	}
	return checkStatusUnknown
}

func watchURL(config urlConfig, events chan<- statusChange) {
	defer exit()

	var lastStatus = checkStatusUnknown
	var history = make([]checkStatus, max(config.OKPeriods, config.AlarmPeriods))

	log.Printf("check %s every %s", config.URL, config.CheckInterval)

	client := &http.Client{
		Timeout:       config.HTTPTimeout.Duration,
		CheckRedirect: ignoreRedirect,
	}

	for {
		resp, err := client.Get(config.URL)
		result := checkResponse(resp, err, config)

		var currentStatus = checkStatusUnknown
		if result {
			currentStatus = checkStatusOk
		} else {
			currentStatus = checkStatusAlarm
		}

		history = append(history[1:], currentStatus)

		newStatus := getNewStatus(history, config.OKPeriods, config.AlarmPeriods)
		if newStatus != checkStatusUnknown && newStatus != lastStatus {
			events <- statusChange{
				name: config.Name,
				url:  config.URL,
				time: time.Now(),
				from: lastStatus,
				to:   newStatus,
			}
			lastStatus = newStatus
		}

		time.Sleep(config.CheckInterval.Duration)
	}
}

func validateURLConfig(config urlConfig) error {
	if utf8.RuneCountInString(config.URL) == 0 {
		return errors.New("empty URL")
	}

	if len(config.OKStatuses) == 0 {
		return errors.New("empty OKStatuses")
	}

	if config.CheckInterval.Seconds() == 0 {
		return errors.New("CheckInterval == 0s")
	}

	if config.HTTPTimeout.Seconds() == 0 {
		return errors.New("HTTPTimeout == 0s")
	}

	if config.AlarmPeriods == 0 {
		return errors.New("AlarmPeriods == 0")
	}

	if config.OKPeriods == 0 {
		return errors.New("OKPeriods == 0")
	}

	return nil
}

func validateConfig(config aliveConfig) error {
	if utf8.RuneCountInString(config.SlackToken) == 0 {
		return errors.New("empty SlackToken")
	}

	if utf8.RuneCountInString(config.SlackChannel) == 0 {
		return errors.New("empty SlackChannel")
	}

	if utf8.RuneCountInString(config.BotName) == 0 {
		return errors.New("empty BotName")
	}

	if len(config.Items) == 0 {
		return errors.New("no items")
	}

	for idx, conf := range config.Items {
		if err := validateURLConfig(conf); err != nil {
			return fmt.Errorf("invalid item %d: %s", idx, err.Error())
		}
	}

	return nil
}

func checkStatusToString(status checkStatus) string {
	switch status {
	case checkStatusAlarm:
		return "alarm"
	case checkStatusOk:
		return "ok"
	default:
		return "unknown"
	}
}

func formatSlackMessage(botName string, change statusChange) slack.PostMessageParameters {
	text := fmt.Sprintf(
		"%s (%s) *%s*",
		change.name,
		change.url,
		strings.ToUpper(checkStatusToString(change.to)),
	)
	messageParams := slack.PostMessageParameters{Username: botName}
	attach := slack.Attachment{}
	attach.Fallback = text
	attach.Text = text
	attach.MarkdownIn = []string{"text"}
	switch change.to {
	case checkStatusOk:
		attach.Color = "good"
	case checkStatusAlarm:
		attach.Color = "danger"
	}
	messageParams.Attachments = []slack.Attachment{attach}
	return messageParams
}

func slackNotifier(token string, channel string, botName string, events <-chan statusChange) {
	defer exit()

	slackAPI := slack.New(token)
	for change := range events {
		log.Printf("%+v", change)

		_, _, err := slackAPI.PostMessage(channel, "", formatSlackMessage(botName, change))
		if err != nil {
			panic(err)
		}
	}
}

func main() {
	var configPath = os.Getenv("ITSALIVE_CONFIG")
	if utf8.RuneCountInString(configPath) == 0 {
		configPath = "itsalive.toml"
	}

	var config aliveConfig
	_, err := toml.DecodeFile(configPath, &config)

	if err != nil {
		panic(err)
	}

	if err := validateConfig(config); err != nil {
		log.Panicf("invalid config: %s", err.Error())
	}

	events := make(chan statusChange, 100)

	go slackNotifier(
		config.SlackToken,
		config.SlackChannel,
		config.BotName,
		events,
	)

	for _, conf := range config.Items {
		go watchURL(conf, events)
	}

	for {
		time.Sleep(time.Second)
	}
}
