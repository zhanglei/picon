package picon

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/chzyer/readline"
	pilosa "github.com/pilosa/go-client-pilosa"
)

type promptInfo struct {
	address  string
	database string
}

type Console struct {
	client            *pilosa.Client
	database          *pilosa.Database
	prompt            *promptInfo
	lastResponse      *pilosa.QueryResponse
	inst              *readline.Instance
	homeDirectory     string
	sessionsDirectory string
	session           []string
	sessionName       string
	schema            *pilosa.Schema
}

func NewConsole(homeDirectory string) (*Console, error) {
	sessionsDirectory := ""
	if homeDirectory != "" {
		sessionsDirectory = path.Join(homeDirectory, "sessions")
	}
	console := &Console{
		prompt:            &promptInfo{address: "(not connected)", database: "(no DB)"},
		homeDirectory:     homeDirectory,
		sessionsDirectory: sessionsDirectory,
		session:           []string{},
		sessionName:       autoSessionName(),
	}
	completer := readline.NewPrefixCompleter(
		readline.PcItem(":exit"),
		readline.PcItem(":connect"),
		readline.PcItem(":use", readline.PcItemDynamic(console.listDatabases())),
		readline.PcItem(":create",
			readline.PcItem("db"),
			readline.PcItem("frame")),
		readline.PcItem(":ensure",
			readline.PcItem("db"),
			readline.PcItem("frame")),
		readline.PcItem(":schema"),
		readline.PcItem(":save"),
		readline.PcItem(":session"),
	)
	config := &readline.Config{
		AutoComplete:      completer,
		InterruptPrompt:   "^C",
		EOFPrompt:         ":exit",
		HistorySearchFold: true,
	}
	if homeDirectory != "" {
		config.HistoryFile = path.Join(homeDirectory, "history")
	}
	inst, err := readline.NewEx(config)
	if err != nil {
		return nil, err
	}
	console.inst = inst
	return console, nil
}

func (c *Console) Close() {
	c.inst.Close()
}

func (c *Console) Main() {
	c.ensureHomeDirectoryExists()
	log.SetOutput(c.inst.Stderr())
	c.updatePrompt()
	lines := []string{}
	for {
		line, err := c.inst.Readline()
		if err == readline.ErrInterrupt {
			if len(line) == 0 {
				break
			} else {
				continue
			}
		}
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "\\") {
			c.inst.SetPrompt(">>> ")
			lines = append(lines, strings.TrimRight(line, "\\"))
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, line)
			line = strings.Join(lines, "\n")
			lines = []string{}
			c.updatePrompt()
		}
		switch {
		case line == "":
			continue
		case line == ":exit":
			goto exit
		case strings.HasPrefix(line, "#"):
			c.inst.Operation.SetBuffer("# ")
		case strings.HasPrefix(line, ":"):
			err = c.executeCommand(line)
		case line == "_":
			if c.lastResponse != nil {
				printResponse(c.lastResponse)
			}
		default:
			err = c.executeQuery(line)
		}

		if err != nil {
			printError(err)
		} else {
			// do not save session commands to the session
			for _, s := range []string{":save", ":load", ":session"} {
				if strings.HasPrefix(line, s) {
					goto session_exit
				}
			}
			c.session = append(c.session, line)
		}
	session_exit:
	}
exit:
}

func (c *Console) listDatabases() func(string) []string {
	return func(line string) []string {
		dbnames := []string{}
		if c.schema != nil {
			for _, db := range c.schema.DBs {
				dbnames = append(dbnames, db.Name)
			}
		}
		return dbnames
	}
}

func (c *Console) executeCommand(line string) (err error) {
	args := strings.Fields(line)
	cmd := args[0]
	switch cmd {
	case ":connect":
		err = c.executeConnectCommand(cmd, args[1:])
	case ":use":
		err = c.executeUseCommand(cmd, args[1:])
	case ":ensure":
		err = c.executeEnsureCommand(cmd, args[1:])
	case ":save":
		err = c.executeSaveCommand(cmd, args[1:])
	case ":session":
		err = c.executeSessionCommand(cmd, args[1:])
	case ":schema":
		err = c.executeSchemaCommand(cmd, args[1:])
	default:
		err = fmt.Errorf("Invalid command: %s", cmd)
	}
	return err
}

func (c *Console) executeConnectCommand(cmd string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: :connect pilosa-address")
	}
	uri, err := pilosa.NewURIFromAddress(args[0])
	if err != nil {
		return err
	}
	c.client = pilosa.NewClientWithAddress(uri)
	err = c.updateSchema()
	if err != nil {
		c.client = nil
		return err
	}
	c.prompt.address = uri.GetNormalizedAddress()
	c.updatePrompt()
	return nil
}

func (c *Console) executeUseCommand(cmd string, args []string) (err error) {
	if len(args) != 1 {
		return errors.New("usage: :use db-name")
	}
	if c.client == nil {
		return errNotConnected
	}
	databaseName := args[0]
	c.database, err = pilosa.NewDatabase(databaseName)
	if err != nil {
		return err
	}
	c.prompt.database = databaseName
	c.updatePrompt()
	return nil
}
func (c *Console) executeEnsureCommand(cmd string, args []string) (err error) {
	if c.client == nil {
		return errNotConnected
	}
	if len(args) != 2 {
		return errors.New("Usage: :ensure db/frame name")
	}

	what := args[1]
	which := args[0]
	switch which {
	case "db":
		databaseName := what
		c.database, err = pilosa.NewDatabase(what)
		if err != nil {
			return err
		}
		err = c.client.EnsureDatabaseExists(c.database)
		if err != nil {
			return err
		}
		c.prompt.database = databaseName
		c.updatePrompt()
		err = c.updateSchema()
	case "frame":
		if c.database == nil {
			return errNoDatabase
		}
		frameName := what
		frame, err := c.database.Frame(frameName)
		if err != nil {
			return err
		}
		err = c.client.EnsureFrameExists(frame)
	default:
		return fmt.Errorf("Don't know how to ensure %s", which)
	}
	return err
}

func (c *Console) executeSaveCommand(cmd string, args []string) error {
	if len(args) > 0 {
		return errors.New("Usage: :save")
	}
	if c.sessionsDirectory == "" {
		return errors.New("session directory was not set")
	}
	path := path.Join(c.sessionsDirectory, c.sessionName)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, line := range c.session {
		_, err := f.WriteString(fmt.Sprintf("%s\n", line))
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Console) executeSessionCommand(cmd string, args []string) error {
	switch len(args) {
	case 0:
		c.sessionName = autoSessionName()
	case 1:
		c.sessionName = args[1]
	default:
		return errors.New("Usage: :session [session name]")
	}
	// reset session
	c.session = []string{}
	return nil
}

func (c *Console) executeSchemaCommand(cmd string, args []string) error {
	// TODO: check number of args
	// 0: schema for all
	// 1: schema for the given db
	if len(args) > 1 {
		return errors.New("usage: :schema [database name | *]")
	}
	dbname := ""
	if len(args) == 1 {
		if args[0] != "*" {
			dbname = args[0]
		}
	} else {
		if c.database != nil {
			dbname = c.database.GetName()
		}
	}
	err := c.updateSchema()
	if err != nil {
		return err
	}
	for _, db := range c.schema.DBs {
		if dbname == "" || dbname == db.Name {
			frameList := make([]string, 0, len(db.Frames))
			for _, frame := range db.Frames {
				frameList = append(frameList, frame.Name)
			}
			if dbname != "" {
				fmt.Printf("[%s]\n", strings.Join(frameList, ", "))
			} else {
				fmt.Printf("%s [%s]\n", db.Name, strings.Join(frameList, ", "))
			}
		}
	}
	return nil
}

func (c *Console) executeQuery(line string) error {
	if c.client == nil {
		return errNotConnected
	}
	if c.database == nil {
		return errNoDatabase
	}
	response, err := c.client.Query(c.database, line)
	if err != nil {
		return err
	}
	c.lastResponse = response
	printResponse(response)
	return nil
}

func (c *Console) updatePrompt() {
	c.inst.SetPrompt(fmt.Sprintf("\033[36m%s\033[0m/\033[1m\033[32m%s\033[0m > ",
		c.prompt.address, c.prompt.database))
}

func (c *Console) ensureHomeDirectoryExists() {
	if c.homeDirectory != "" {
		err := os.MkdirAll(c.homeDirectory, 0700)
		if err != nil {
			c.homeDirectory = ""
			printWarning(fmt.Sprintf("Cannot create %s, unsetting it.", c.homeDirectory))
			return
		}
	}
	if c.sessionsDirectory != "" {
		err := os.MkdirAll(c.sessionsDirectory, 0700)
		if err != nil {
			c.sessionsDirectory = ""
			printWarning(fmt.Sprintf("Cannot create %s, unsetting it.", c.sessionsDirectory))
		}
	}
}

func (c *Console) updateSchema() error {
	if c.client == nil {
		return errNotConnected
	}
	schema, err := c.client.Schema()
	if err != nil {
		return err
	}
	c.schema = schema
	return nil
}

func printResponse(response *pilosa.QueryResponse) {
	if !response.IsSuccess {
		printError(errors.New(response.ErrorMessage))
		return
	}
	results := response.Results
	if results != nil {
		for i, result := range results {
			printResult(i, len(response.Results), result)
		}
	}
}

func printError(err error) {
	fmt.Println(colorString(fgRed, err.Error()))
}

func printWarning(msg string) {
	fmt.Println(colorString(fgRed, msg))
}

func colorString(color Ansi, msg string) string {
	return fmt.Sprintf("%s%s%s", color, msg, attrReset)
}

func printResult(index int, count int, result *pilosa.QueryResult) {
	headerFmt := fmt.Sprintf("[%%%dd] --------", int(math.Ceil(float64(count)/10.0)))
	lines := []string{fmt.Sprintf(headerFmt, index)}
	canPrint := false
	switch {
	case result.BitmapResult != nil:
		if len(attributesToString(result.BitmapResult.Attributes)) > 0 {
			lines = append(lines,
				fmt.Sprintf("\tAttributes: %s", attributesToString(result.BitmapResult.Attributes)))
			canPrint = true
		}
		if len(bitsToString(result.BitmapResult.Bits)) > 0 {
			lines = append(lines,
				fmt.Sprintf("\tBits      : %s", bitsToString(result.BitmapResult.Bits)))
			canPrint = true
		}
	case result.CountItems != nil && len(result.CountItems) > 0:
		for _, item := range result.CountItems {
			lines = append(lines, fmt.Sprintf("\tCount(%d) = %d\n", item.ID, item.Count))
			canPrint = true
		}
	case result.Count > 0:
		lines = append(lines, fmt.Sprintf("\tCount: %d\n", result.Count))
		canPrint = true
	}
	if canPrint {
		fmt.Println(strings.Join(lines, "\n"))
	}
}

func attributesToString(attrs map[string]interface{}) string {
	parts := make([]string, 0, len(attrs))
	for k, v := range attrs {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, ", ")
}

func bitsToString(bits []uint64) string {
	parts := make([]string, 0, len(bits))
	for _, v := range bits {
		parts = append(parts, strconv.Itoa(int(v)))
	}
	return strings.Join(parts, ", ")
}

func autoSessionName() string {
	return time.Now().Format("2006-01-02_15-04-05")
}