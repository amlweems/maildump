package main

import "net"
import "fmt"
import "log"
import "flag"
import "path"
import "strings"
import "regexp"
import "io"
import "io/ioutil"
import "os"
import "time"

type ReplyCode string
type Command int

const (
	ReplyServiceReady          ReplyCode = "220 mail.lf.lc ESMTP dumptruck"
	ReplyServiceClosing        ReplyCode = "221 goodbye"
	ReplyOkay                  ReplyCode = "250 yes sir"
	ReplyStartMailInput        ReplyCode = "354 fill 'er up"
	ReplyServiceNotAvailable   ReplyCode = "421 not at the moment"
	ReplyCommandNotImplemented ReplyCode = "502 *shrugs*"
)

const (
	CommandEhlo Command = iota
	CommandHelo
	CommandMail
	CommandRcpt
	CommandData
	CommandRset
	CommandVrfy
	CommandExpn
	CommandHelp
	CommandNoop
	CommandQuit
)

var replyTable = map[Command]ReplyCode{
	CommandEhlo: ReplyOkay,
	CommandMail: ReplyOkay,
	CommandRcpt: ReplyOkay,
	CommandData: ReplyStartMailInput,
	CommandRset: ReplyOkay,
	CommandVrfy: ReplyOkay,
	CommandExpn: ReplyCommandNotImplemented,
	CommandHelp: ReplyCommandNotImplemented,
	CommandNoop: ReplyOkay,
	CommandQuit: ReplyServiceClosing,
}

var commandTable = map[string]Command{
	"EHLO": CommandEhlo,
	"HELO": CommandEhlo,
	"MAIL": CommandMail,
	"RCPT": CommandRcpt,
	"DATA": CommandData,
	"RSET": CommandRset,
	"VRFY": CommandVrfy,
	"EXPN": CommandExpn,
	"HELP": CommandHelp,
	"NOOP": CommandNoop,
	"QUIT": CommandQuit,
}

func readCommand(conn net.Conn, buf []byte) (int, error) {
	datum := make([]byte, 1)
	length := 0
	for {
		bytesRead, err := conn.Read(datum)
		if err != nil {
			return 0, err
		}
		if bytesRead == 1 && length < cap(buf) {
			buf[length] = datum[0]
			length += bytesRead
			if datum[0] == '\n' {
				return length, nil
			}
		}
	}
}

func replyCommand(conn net.Conn, line string) Command {
	line = strings.TrimSpace(line)
	args := strings.Split(line, " ")
	cmd, exists := commandTable[strings.ToUpper(args[0])]
	if exists {
		reply, exists := replyTable[cmd]
		if exists {
			fmt.Fprintln(conn, reply)
		} else {
			fmt.Fprintln(conn, ReplyCommandNotImplemented)
		}
	} else {
		fmt.Fprintln(conn, ReplyOkay)
	}
	return cmd
}

func toIPAddress(addr net.Addr) string {
	ipAddress := strings.Split(addr.String(), ":")[0]
	dots := strings.Split(ipAddress, ".")

	/* https://stackoverflow.com/questions/34816489/reverse-slice-of-strings */
	last := len(dots) - 1
	for i := 0; i < len(dots)/2; i++ {
		dots[i], dots[last-i] = dots[last-i], dots[i]
	}

	return strings.Join(dots, ".")
}

var serverBlocklist = []string{".zen.spamhaus.org", ".bl.spamcop.net", ".b.barracudacentral.org", ".dnsbl.sorbs.net"}

func isSpammerAddr(addr net.Addr) bool {
	ipAddress := toIPAddress(addr)
	for _, server := range serverBlocklist {
		_, err := net.LookupHost(ipAddress + server)
		if err == nil {
			return true
		}
	}
	return false
}

var defaultAddr = "invalid@addr"

func sanitizeAddr(dirty string) string {
	re := regexp.MustCompile("(MAIL|RCPT) (FROM|TO|From|To):.*<([^>]+)>")
	subs := re.FindAllStringSubmatch(dirty, 1)
	if subs != nil && len(subs) > 0 && len(subs[0]) == 4 && len(subs[0][3]) > 0 {
		re = regexp.MustCompile("[^a-zA-Z0-9@]+")
		addr := subs[0][3]
		return re.ReplaceAllString(addr, ".")
	} else {
		return defaultAddr
	}
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	cerr := out.Close()
	if err != nil {
		return err
	}
	return cerr
}

var messageNameFormat = "%v-%v-%v.txt"

func handleConn(conn net.Conn) {
	defer conn.Close()

	if spamDetection && isSpammerAddr(conn.RemoteAddr()) {
		fmt.Printf("discarding mail from %v\n", conn.RemoteAddr())
		return
	} else {
		fmt.Printf("receiving mail from %v\n", conn.RemoteAddr())
	}

	output, err := ioutil.TempFile("/tmp", "maildump")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer output.Close()
	defer os.Remove(output.Name())

	var toAddr = defaultAddr
	remoteIP := toIPAddress(conn.RemoteAddr())

	_, err = conn.Write([]byte("220 mail.lf.lc ESMTP dumptruck\n"))
	if err != nil {
		fmt.Println(err)
		return
	}

	rawData := make([]byte, 1024)
	readingData := false

CommandParse:
	for {
		bytesRead, err := readCommand(conn, rawData)
		if err != nil {
			break
		}
		output.Write(rawData[:bytesRead])

		if readingData && rawData[0] == '.' {
			readingData = false
		}

		if !readingData {
			data := string(rawData[:bytesRead])
			cmd := replyCommand(conn, data)
			switch cmd {
			case CommandMail:
				break
			case CommandRcpt:
				toAddr = sanitizeAddr(data)
				break
			case CommandData:
				readingData = true
				break
			case CommandQuit:
				break CommandParse
			}
		}
	}
	output.Sync()

	stats, err := output.Stat()
	output.Close()
	if err != nil {
		fmt.Println(err)
		return
	}
	if stats.Size() > 50 {
		messageName := fmt.Sprintf(messageNameFormat, toAddr, remoteIP, time.Now().Unix())
		mailPath := path.Join(outputDirectory, messageName)
		err = copyFileContents(output.Name(), mailPath)
		if err != nil {
			fmt.Println(err)
			return
		}
	}
}

var outputDirectory string
var listeningPort string
var spamDetection bool

func main() {
	flag.StringVar(&outputDirectory, "output", "/srv/http/maildump", "output directory for mail")
	flag.StringVar(&listeningPort, "port", ":25", "listening port")
	flag.BoolVar(&spamDetection, "spam", true, "perform spam detection")
	flag.Parse()

	err := os.MkdirAll(outputDirectory, 0400)
	if err != nil {
		log.Fatal(err)
	}

	ln, err := net.Listen("tcp", listeningPort)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Listening on", listeningPort)
	fmt.Println("Spam detection: ", spamDetection)
	fmt.Println("Output directory: ", outputDirectory)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Println(err)
		}
		go handleConn(conn)
	}
}
