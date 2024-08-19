package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus"
	"github.com/spf13/viper"
	"github.com/srajelli/ses-go"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	envServices = "crond.service, nginx.service"

	propertiesChanged = "org.freedesktop.DBus.Properties.PropertiesChanged"
	propGet           = "org.freedesktop.DBus.Properties.Get"
)

type serviceStatus struct {
	ID                     string
	Name                   string
	Path                   dbus.ObjectPath
	ActiveState            string
	ActiveEnterTimestamp   uint64
	ActiveExitTimestamp    uint64
	InactiveEnterTimestamp uint64
	InactiveExitTimestamp  uint64
}

type SlackRequestBody struct {
	Text string `json:"text"`
}

func formatTime(us uint64) string {
	return time.Unix(0, 1000*int64(us)).Format(time.RFC1123Z)
}

func (s serviceStatus) String() string {
	var buf bytes.Buffer

	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	buf.WriteString(fmt.Sprintf("IP: %s\n", s.ID))
	buf.WriteString(fmt.Sprintf("Hostname: %s\n", hostname))
	buf.WriteString(fmt.Sprintf("Service Name: %s\n", s.Name))
	buf.WriteString(fmt.Sprintf("State: %s\n", s.ActiveState))
	//buf.WriteString(fmt.Sprintf("ActiveEnterTime: %s\n", formatTime(s.ActiveEnterTimestamp)))
	//buf.WriteString(fmt.Sprintf("ActiveExitTime: %s\n", formatTime(s.ActiveExitTimestamp)))
	//buf.WriteString(fmt.Sprintf("InactiveEnterTime: %s\n", formatTime(s.InactiveEnterTimestamp)))
	//buf.WriteString(fmt.Sprintf("InactiveExitTime: %s\n", formatTime(s.InactiveExitTimestamp)))

	return buf.String()
}

type unitMonitor struct {
	name    string
	done    chan struct{}
	path    dbus.ObjectPath
	chanPub chan serviceStatus
}

func (m *unitMonitor) getUnitPath() dbus.ObjectPath {

	var ret dbus.ObjectPath

	c, err := dbus.SystemBus()
	if err != nil {
		return ret
	}

	obj := c.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")
	call := obj.Call("org.freedesktop.systemd1.Manager.GetUnit", 0, m.name)
	if call != nil && call.Err != nil {
		fmt.Println(err)
		return ret
	}

	if err := call.Store(&ret); err != nil {
		fmt.Println(err)
		return ret
	}

	return ret
}

func (m *unitMonitor) getProp(name string) dbus.Variant {

	var ret dbus.Variant

	c, err := dbus.SystemBus()
	if err != nil {
		fmt.Println(err)
		return ret
	}

	err = c.Object("org.freedesktop.systemd1", m.path).Call(
		propGet,
		0,
		"org.freedesktop.systemd1.Unit",
		name,
	).Store(&ret)

	if err != nil {
		fmt.Println(err)
	}
	return ret
}

func (m *unitMonitor) generateStatus() serviceStatus {
	status := serviceStatus{
		ID:   getLocalIP(),
		Name: m.name,
		Path: m.path,
	}

	var prop dbus.Variant
	prop = m.getProp("ActiveState")
	if prop.Value() != nil {
		status.ActiveState = prop.Value().(string)
	}

	prop = m.getProp("ActiveEnterTimestamp")
	if prop.Value() != nil {
		status.ActiveEnterTimestamp = prop.Value().(uint64)
	}

	prop = m.getProp("ActiveExitTimestamp")
	if prop.Value() != nil {
		status.ActiveExitTimestamp = prop.Value().(uint64)
	}

	prop = m.getProp("InactiveEnterTimestamp")
	if prop.Value() != nil {
		status.InactiveEnterTimestamp = prop.Value().(uint64)
	}

	prop = m.getProp("InactiveExitTimestamp")
	if prop.Value() != nil {
		status.InactiveExitTimestamp = prop.Value().(uint64)
	}

	return status
}

func (m *unitMonitor) watch() error {

	conn, err := dbus.SystemBus()
	if err != nil {
		return err
	}

	m.path = m.getUnitPath()

	// subscribe the props changes signal
	props := fmt.Sprintf("type='signal',path='%s',interface='org.freedesktop.DBus.Properties'", m.path)
	call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, props)
	if call != nil && call.Err != nil {
		return call.Err
	}

	// unsubscribe on return
	defer func() {
		conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, props)
	}()

	signal := make(chan *dbus.Signal, 10)
	defer close(signal)

	conn.Signal(signal)

	fmt.Printf("watching for %s @%s\n", m.name, m.path)

	for {
		select {
		case ev := <-signal:
			switch ev.Name {
			case propertiesChanged:
				var iName string
				var changedProps map[string]dbus.Variant
				var invProps []string

				if ev.Path == m.path {
					if err := dbus.Store(ev.Body, &iName, &changedProps, &invProps); err != nil {
						fmt.Println(err.Error())
						continue
					}

					if iName == "org.freedesktop.systemd1.Unit" {
						m.chanPub <- m.generateStatus()
					}
				}
			}

		case <-m.done:
			return nil

		}
	}

}

func waitForOsSignal() {
	signals := []os.Signal{syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGSTOP}
	signalSink := make(chan os.Signal, 1)
	defer close(signalSink)

	signal.Notify(signalSink, signals...)
	<-signalSink
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		// check the address type and if it is not a loopback the display it
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func watchServices(chanDone chan struct{}, units ...string) {
	chanPub := make(chan serviceStatus, 100)
	defer close(chanPub)

	for _, unit := range units {
		go func(u string) {
			m := &unitMonitor{
				done:    chanDone,
				name:    u,
				chanPub: chanPub,
			}

			if err := m.watch(); err != nil {
				fmt.Println(u, err)
			}

		}(unit)
	}

	for {
		select {
		case status := <-chanPub:
			// TODO: this is my personal implementation only. Please modify to suit your needs
			contents, _ := ioutil.ReadFile("filename.txt")
			ioutil.WriteFile("filename.txt", []byte(status.ActiveState), 0644)
			if status.ActiveState != string(contents) {
				fmt.Println(status.String())
				if viper.GetBool("app.smtp.enabled") == true && viper.GetBool("app.smtp.ses.enabled") == true {
					//sesAws(status.String())
					to := viper.Get("app.smtp.recipient").(string)
					dest := strings.Split(to, ", ")
					start := 0
					for i := 0; i < len(dest); i++ {
						start += i
						sesAws(dest[start], status.String())
					}
				}
				if viper.GetBool("app.smtp.enabled") == true {
					sendEmail(status.String())
				}
				if viper.GetBool("app.slack.enabled") == true {
					webhook := viper.Get("app.slack.webhook_uri").(string)
					err := slackNotif(webhook, status.String())
					if err != nil {
						log.Fatal(err)
					}
				}
				if viper.GetBool("app.telegram.enabled") == true {
					telegramNotif(status.String())
				}
			}
		case <-chanDone:
			return
		}
	}
}

func sendEmail(body string) {
	from := viper.Get("app.smtp.user").(string)
	pass := viper.Get("app.smtp.password").(string)
	port := viper.Get("app.smtp.port").(string)
	server := viper.Get("app.smtp.server").(string)
	to := viper.Get("app.smtp.recipient").(string)
	subject := viper.Get("app.smtp.subject").(string)

	msg := "From: " + from + "\n" +
		"To: " + to + "\n" +
		"Subject: " + subject + "\n\n" +
		body

	err := smtp.SendMail(server+":"+port,
		smtp.PlainAuth("", from, pass, server),
		from, strings.Split(to, ", "), []byte(msg))

	if err != nil {
		fmt.Printf("smtp error: %s", err)
		return
	}
}

func sesAws(to string, body string) {
	from := viper.Get("app.smtp.user").(string)
	subject := viper.Get("app.smtp.subject").(string)
	awsKeyID := viper.Get("app.smtp.ses.aws-key-id").(string)
	awsSecretKey := viper.Get("app.smtp.ses.aws-secret-key").(string)
	awsRegion := viper.Get("app.smtp.ses.aws-region").(string)

	ses.SetConfiguration(awsKeyID, awsSecretKey, awsRegion)

	emailData := ses.Email{
		To:      to,
		From:    from,
		Text:    body,
		Subject: subject,
		ReplyTo: from,
	}

	resp := ses.SendEmail(emailData)

	fmt.Println(resp)
}

func slackNotif(webhookUrl string, msg string) error {

	slackBody, _ := json.Marshal(SlackRequestBody{Text: msg})
	req, err := http.NewRequest(http.MethodPost, webhookUrl, bytes.NewBuffer(slackBody))
	if err != nil {
		return err
	}

	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	if buf.String() != "ok" {
		return errors.New("Non-ok response returned from Slack")
	}
	return nil
}

func telegramNotif(msg string) error {

	botToken := viper.Get("app.telegram.token").(string)

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = true

	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, msg)
		msg.ReplyToMessageID = update.Message.MessageID

		bot.Send(msg)
	}
	return nil
}

func main() {
	done := make(chan struct{})
	defer close(done)

	// config
	viper.SetConfigName("config") // name of config file (without extension)
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./")
	err := viper.ReadInConfig()
	if err != nil {
		panic(fmt.Errorf("fatal error config file: %s", err))
	}

	// sample services
	units := []string{"teamviewerd.service"}

	ose := viper.Get("app.monitored-service").(string)
	if ose != "" {
		units = []string{}
		for _, s := range strings.Split(ose, ",") {
			units = append(units, strings.TrimSpace(s))
		}
	}

	if len(units) == 0 {
		os.Exit(-1)
	}

	go watchServices(done, units...)
	waitForOsSignal()
}
