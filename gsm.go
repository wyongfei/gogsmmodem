package gogsmmodem

import (
	"bufio"
	"errors"
	"io"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/tarm/goserial"
)

type Modem struct {
	OOB     chan Packet
	Debug   bool
	port    io.ReadWriteCloser
	rx      chan Packet
	tx      chan string
	timeout int //in second
}

var OpenPort = func(config *serial.Config) (io.ReadWriteCloser, error) {
	return serial.OpenPort(config)
}

func Open(config *serial.Config, debug bool) (*Modem, error) {
	port, err := OpenPort(config)
	if debug {
		port = LogReadWriteCloser{port}
	}
	if err != nil {
		return nil, err
	}
	oob := make(chan Packet, 16)
	rx := make(chan Packet)
	tx := make(chan string)
	timeout := 3
	modem := &Modem{
		OOB:     oob,
		Debug:   debug,
		port:    port,
		rx:      rx,
		tx:      tx,
		timeout: timeout,
	}
	// run send/receive goroutine
	go modem.listen()

	err = modem.init()
	if err != nil {
		return nil, err
	}
	return modem, nil
}

func (self *Modem) Close() error {
	close(self.OOB)
	close(self.rx)
	// close(self.tx)
	return self.port.Close()
}

// Commands

// GetMessage by index n from memory.
func (self *Modem) GetMessage(n int) (*Message, error) {
	packet, err := self.send("+CMGR", n)
	if err != nil {
		return nil, err
	}
	if msg, ok := packet.(Message); ok {
		return &msg, nil
	}
	return nil, errors.New("Message not found")
}

// ListMessages stored in memory. Filter should be "ALL", "REC UNREAD", "REC READ", etc.
func (self *Modem) ListMessages(filter string) (*MessageList, error) {
	packet, err := self.send("+CMGL", filter)
	if err != nil {
		return nil, err
	}
	res := MessageList{}
	if _, ok := packet.(OK); ok {
		// empty response
		return &res, nil
	}

	for {
		if msg, ok := packet.(Message); ok {
			res = append(res, msg)
			if msg.Last {
				break
			}
		} else {
			return nil, errors.New("Unexpected error")
		}

		packet = <-self.rx
	}
	return &res, nil
}

func (self *Modem) SupportedStorageAreas() (*StorageAreas, error) {
	packet, err := self.send("+CPMS", "?")
	if err != nil {
		return nil, err
	}
	if msg, ok := packet.(StorageAreas); ok {
		return &msg, nil
	}
	return nil, errors.New("Unexpected response type")
}

func (self *Modem) DeleteMessage(n int) error {
	_, err := self.send("+CMGD", n)
	return err
}

func (self *Modem) SendMessage(telephone, body string) error {
	enc := gsmEncode(body)
	_, err := self.sendBody("+CMGS", enc, telephone)
	return err
}

func lineChannel(r io.Reader) chan string {
	ret := make(chan string)
	go func() {
		buffer := bufio.NewReader(r)
		for {
			line, _ := buffer.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				//continue
			}
			ret <- line
		}
	}()
	return ret
}

var reQuestion = regexp.MustCompile(`AT(\+[A-Z]+)`)

func parsePacket(status string, header string, body string) Packet {
	if header == "" && (status == "OK" || status == "ERROR") {
		if status == "OK" {
			return OK{}
		} else {
			return ERROR{}
		}
	}

	ls := strings.SplitN(header, ":", 2)

	log.Println("PARSE:", ls[0], "-", ls[1])

	if len(ls) != 2 {
		return UnknownPacket{header, []interface{}{}}
	}
	uargs := strings.TrimSpace(ls[1])
	args := unquotes(uargs)
	switch ls[0] {
	case "FACT":
		// message storage unset nag, ignore
		log.Println("FACK RESPONSE:", args)
		return OK{}
	case "":
		if status == "OK" {
			return OK{}
		} else {
			return ERROR{}
		}
	}
	return UnknownPacket{ls[0], args}
}

func (self *Modem) listen() {
	log.Println("LISTEN")
	in := lineChannel(self.port)
	var echo, last, header, body string
	log.Println(time.Now)

	for {
		select {
		case line := <-in:
			log.Println("GET_RESPONSE:", line)
			if line == echo {
				log.Println("ECHO BACK")
				continue // ignore echo of command
			} else if last != "" && startsWith(line, last) {
				// first of multiple responses (eg CMGL)
				packet := parsePacket("", line, "")
				self.rx <- packet
				log.Println("-------")
			} else if line == "OK" || line == "ERROR" {
				packet := parsePacket(line, header, body)
				self.rx <- packet
				header = ""
				body = ""
			} else if header != "" {
				// the body following a header
				body += line
			} else if line == "> " {
				// raw mode for body
			} else {
				// OOB packet
				p := parsePacket("OK", line, "")
				if p != nil {
					self.OOB <- p
				}
			}
		case line := <-self.tx:
			log.Println("SEND_CMD:" + line)
			m := reQuestion.FindStringSubmatch(line)
			if len(m) > 0 {
				last = strings.TrimPrefix(m[1], "+")
				log.Println("LAST CMD:", last)
			}
			echo = strings.TrimRight(line, "\r\n")
			self.port.Write([]byte(line))
		}
	}
}

func formatCommand(cmd string, args ...interface{}) string {
	line := "AT" + cmd
	if len(args) > 0 {
		line += "=" + quotes(args)
	}
	line += "\r\n"
	return line
}

func (self *Modem) sendBody(cmd string, body string, args ...interface{}) (Packet, error) {
	self.tx <- formatCommand(cmd, args...)
	self.tx <- body + "\x1a"
	response := <-self.rx
	if _, e := response.(ERROR); e {
		return response, errors.New("Response was ERROR")
	}

	log.Println(response)

	return response, nil
}

func (self *Modem) send(cmd string, args ...interface{}) (Packet, error) {
	self.tx <- formatCommand(cmd, args...)
	var response Packet

	select {
	case response := <-self.rx:
		log.Println(response)
		return response, nil
	case <-time.After(time.Second * time.Duration(self.timeout)):
		{
			return nil, errors.New("Wait Response timeout!!")
		}
	}

	return response, nil
}

func (self *Modem) init() error {
	// clear settings
	println("===init===")
	if _, err := self.send("+FACT"); err != nil {
		log.Println("ERR", err)
	}

	log.Println("Reset")

	if _, err := self.send("+FCT"); err != nil {
		log.Println("ERR")
	}
	log.Println("Test")

	return nil
}
