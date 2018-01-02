package bot

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/jmoiron/sqlx"
	"github.com/thoj/go-ircevent"
)

var logger *log.Logger

func init() {
	logger = log.New(os.Stdout, "", log.LstdFlags)
}

type Channel struct {
	Users map[string]struct{}
	Name  string
	Votes map[string]string
}

func (c *Channel) AddUser(name string) {
	c.Users[name] = struct{}{}
}

func (c *Channel) HasUser(name string) bool {
	_, ok := c.Users[name]
	return ok
}

type Server struct {
	channels map[string]*Channel
	prefixes string
}

func (s *Server) HandleISUPPORT(parts []string) {
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if kv[0] == "PREFIX" {
			s.prefixes = strings.SplitN(kv[1], ")", 2)[1]
			logger.Printf("ISUPPORT setting prefixes to: %v", s.prefixes)
		}
		logger.Printf("ISUPPORT %s => kv: %v", part, kv)
	}
}

func (s *Server) StripUserPrefix(name string) string {
	return strings.TrimLeft(name, s.prefixes)
}

func (s *Server) ChangeNick(from, to string, channel_callback func(string)) {
	for _, channel := range s.channels {
		if channel.HasUser(from) {
			channel.AddUser(to)
			channel_callback(channel.Name)
		}
	}
}

func (s *Server) GetChan(name string) *Channel {
	if channel, ok := s.channels[name]; ok {
		return channel
	}
	s.channels[name] = &Channel{map[string]struct{}{}, name, map[string]string{}}
	return s.channels[name]
}

func DBFromContext(ctx context.Context) *sqlx.DB {
	return ctx.Value("dbcon").(*sqlx.DB)
}

func IsMaster(ctx context.Context, nick string) bool {
	return ctx.Value("master").(string) == nick
}

func addUser(e *irc.Event, db *sqlx.DB, nick, channel string) {
	db.Exec("INSERT INTO karma (server, channel, nick, value) VALUES(?, ?, ?, 0)", e.Connection.Server, channel, nick)
}

func updateKarma(con *irc.Connection, db *sqlx.DB, nick, channel string, value int) {
	_, e := db.Exec("INSERT INTO karma (server, channel, nick, value) VALUES(?, ?, ?, ?)", con.Server, channel, nick, value)
	if e != nil {
		_, e = db.Exec("UPDATE karma SET value = value + ? WHERE server = ? AND channel = ? AND nick = ?", value, con.Server, channel, nick)
	}

	if e != nil {
		logger.Printf("Insert failed: %s", e.Error())
	} else if db.Get(&value, "SELECT value FROM karma WHERE server = ? AND channel = ? AND nick = ?", con.Server, channel, nick) == nil {
		con.Privmsg(channel, fmt.Sprintf("%s has now a karma of %d", nick, value))
	}
}

func addKarma(e *irc.Event, target string, ctx context.Context) {
	if ctx.Value("server").(*Server).GetChan(e.Arguments[0]).HasUser(target) {
		db := DBFromContext(ctx)
		updateKarma(e.Connection, db, target, e.Arguments[0], 1)
	}
}

func subKarma(e *irc.Event, target string, ctx context.Context) {
	if ctx.Value("server").(*Server).GetChan(e.Arguments[0]).HasUser(target) {
		db := DBFromContext(ctx)
		updateKarma(e.Connection, db, target, e.Arguments[0], -1)
	}
}

func CallbackWithContext(ctx context.Context) func(f func(context.Context, *irc.Event)) func(*irc.Event) {
	return func(f func(context.Context, *irc.Event)) func(*irc.Event) {
		return func(event *irc.Event) {
			f(ctx, event)
		}
	}
}

type Connection struct {
	Master   string `db:"master"`
	Server   string `db:"server"`
	Channels string `db:"channels"`
	Nick     string `db:"nick"`
}

func Run(args []string) {
	waitable := []chan struct{}{}
	ctx := context.Background()
	db, err := sql.Open("sqlite3", os.ExpandEnv("$HOME/.evilkarma.db"))
	if err != nil {
		logger.Printf("Failed to open db! %s", err.Error())
		panic(err)
	} else if db == nil {
		panic("WTF")
	} else {
		db.Exec(`
		CREATE TABLE IF NOT EXISTS connections(
			master TEXT NOT NULL,
			server TEXT NOT NULL,
			channels TEXT NOT NULL,
			nick TEXT NOT NULL
		)`)
		db.Exec(`
		CREATE TABLE IF NOT EXISTS karma(
			server TEXT NOT NULL,
			channel TEXT NOT NULL,
			nick TEXT NOT NULL,
			value INT NOT NULL DEFAULT 0,
			PRIMARY KEY(server, channel, nick)
		)`)
	}
	dbx := sqlx.NewDb(db, "sqlite3")
	var connections []Connection
	dbx.Select(&connections, "SELECT * FROM connections")
	for _, connectionConf := range connections {
		ctx = context.WithValue(ctx, "server", &Server{map[string]*Channel{}, "!@+%"})
		ctx = context.WithValue(ctx, "dbcon", dbx)
		ctx = context.WithValue(ctx, "master", connectionConf.Master)

		connection := irc.IRC(connectionConf.Nick, connectionConf.Master)
		ctx = context.WithValue(ctx, "connection", connection)

		ctxCallback := CallbackWithContext(ctx)
		connection.AddCallback("001", func(e *irc.Event) {
			for _, name := range strings.Split(connectionConf.Channels, ",") {
				connection.Join(name)
			}
		})

		connection.AddCallback("005", ctxCallback(func(ctx context.Context, e *irc.Event) {
			server := ctx.Value("server").(*Server)
			server.HandleISUPPORT(e.Arguments[1:])
		}))

		connection.AddCallback("353", ctxCallback(func(ctx context.Context, e *irc.Event) {
			server := ctx.Value("server").(*Server)
			db := DBFromContext(ctx)
			channel := server.GetChan(e.Arguments[2])
			if len(e.Arguments) == 4 {
				for _, nick := range strings.Split(e.Arguments[3], " ") {
					nick = server.StripUserPrefix(nick)
					channel.AddUser(nick)
					addUser(e, db, nick, e.Arguments[2])
				}
			}
			logger.Printf("Loading historical data for channel '%s'...", e.Arguments[2])
			var names []string
			if db.Select(&names, "SELECT nick FROM karma WHERE server = ? AND channel = ?", e.Connection.Server, e.Arguments[2]) == nil {
				for _, name := range names {
					channel.AddUser(name)
				}
			}
			logger.Printf("Added channel: %s -> %v", e.Arguments[2], channel)
		}))

		connection.AddCallback("JOIN", ctxCallback(func(ctx context.Context, e *irc.Event) {
			server := ctx.Value("server").(*Server)
			e.Nick = server.StripUserPrefix(e.Nick)
			if len(e.Arguments) == 1 {
				server.GetChan(e.Arguments[0]).AddUser(e.Nick)
				addUser(e, DBFromContext(ctx), e.Nick, e.Arguments[0])
			}
			logger.Printf("%s JOINED %s", e.Nick, e.Arguments[0])
		}))

		connection.AddCallback("NICK", ctxCallback(func(ctx context.Context, e *irc.Event) {
			server := ctx.Value("server").(*Server)
			if len(e.Arguments) == 1 {
				server.ChangeNick(e.Nick, e.Arguments[0], func(channel string) {
					addUser(e, DBFromContext(ctx), e.Arguments[0], channel)
				})
			}
			logger.Printf("%s RENAMED THEMSELVES TO %s", e.Nick, e.Arguments[0])
		}))

		connection.AddCallback("PRIVMSG", ctxCallback(func(ctx context.Context, e *irc.Event) {
			msg := strings.TrimSpace(e.Message())
			server := ctx.Value("server").(*Server)
			e.Nick = server.StripUserPrefix(e.Nick)
			parts := strings.SplitAfter(msg, "++")
			if strings.HasSuffix(parts[0], "++") {
				name := strings.Trim(parts[0], "+ :")
				if len(name) == 0 {
					rest := strings.Split(parts[1], strings.Trim(parts[1], "+ :"))
					if len(rest) > 0 {
						name = rest[0]
					}
				}
				addKarma(e, name, ctx)
				return
			}
			parts = strings.SplitAfter(msg, "--")
			if strings.HasSuffix(parts[0], "--") {
				name := strings.Trim(parts[0], "- :")
				if len(name) == 0 {
					rest := strings.Split(parts[1], strings.Trim(parts[1], "- :"))
					if len(rest) > 0 {
						name = rest[0]
					}
				}
				subKarma(e, name, ctx)
				return
			}
			if e.Nick == "vfeenstr" || e.Nick == "evilissimo" && len(e.Arguments) == 1 {
				if strings.HasPrefix(strings.ToLower(strings.TrimSpace(e.Message())), "quit") {
					if len(strings.TrimSpace(e.Message())) > 4 {
						e.Connection.QuitMessage = strings.TrimSpace(strings.TrimSpace(e.Message())[4:])
						logger.Printf("Setting new QuitMessage to `%s`\n", e.Connection.QuitMessage)
					}
					e.Connection.Quit()
				} else if strings.HasPrefix(strings.ToLower(msg), "raw: ") {
					e.Connection.SendRaw(e.Message()[5:])
				}
			}
			if strings.HasPrefix(msg, e.Connection.GetNick()+":") {
				msg = strings.TrimSpace(msg[len(e.Connection.GetNick()+":"):])
				if strings.ToLower(msg) == "!votestart" {
				}
			}
		}))

		connection.VerboseCallbackHandler = true
		connection.Debug = true
		err = connection.Connect(connectionConf.Server)
		if err == nil {
			notify := make(chan struct{})
			waitable = append(waitable, notify)
			go func(notify chan struct{}) {
				defer close(notify)
				connection.Loop()
			}(notify)
		} else {
			logger.Printf("Failed to connect: %s \n", err.Error())
			return
		}
	}
	for _, channel := range waitable {
		<-channel
	}
}
