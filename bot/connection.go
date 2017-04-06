package bot

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/jmoiron/sqlx"
	"github.com/thoj/go-ircevent"
)

type Channel struct {
	Users map[string]struct{}
	Name  string
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
}

func (s *Server) GetChan(name string) *Channel {
	if channel, ok := s.channels[name]; ok {
		return channel
	}
	s.channels[name] = &Channel{map[string]struct{}{}, name}
	return s.channels[name]
}

func DBFromContext(ctx context.Context) *sql.DB {
	return ctx.Value("dbcon").(*sql.DB)
}

func updateKarma(con *irc.Connection, db *sql.DB, nick, channel string, value int) {
	dbx := sqlx.NewDb(db, "sqlite3")
	_, e := db.Exec("INSERT INTO karma (server, channel, nick, value) VALUES(?, ?, ?, ?)", con.Server, channel, nick, value)
	if e != nil {
		_, e = db.Exec("UPDATE karma SET value = value + ? WHERE server = ? AND channel = ? AND nick = ?", value, con.Server, channel, nick)
	}

	if e != nil {
		fmt.Printf("Insert failed: %s", e.Error())
	} else if dbx.Get(&value, "SELECT value FROM karma WHERE server = ? AND channel = ? AND nick = ?", con.Server, channel, nick) == nil {
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

func Run() {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", os.ExpandEnv("$HOME/.evilkarma.db"))
	if err != nil {
		fmt.Printf("Failed to open db! %s", err.Error())
		panic(err)
	} else if db == nil {
		panic("WTF")
	} else {
		db.Exec(`CREATE TABLE IF NOT EXISTS karma(
			server TEXT NOT NULL,
			channel TEXT NOT NULL,
			nick TEXT NOT NULL,
			value INT NOT NULL DEFAULT 0,
			PRIMARY KEY(server, channel, nick)
		)
		`)
	}
	ctx = context.WithValue(ctx, "server", &Server{map[string]*Channel{}})
	ctx = context.WithValue(ctx, "dbcon", db)

	connection := irc.IRC("evilkarma", "vfeenstr")
	ctx = context.WithValue(ctx, "connection", connection)

	ctxCallback := CallbackWithContext(ctx)
	connection.AddCallback("001", func(e *irc.Event) {
		connection.Join("#bot-test")
	})

	connection.AddCallback("353", ctxCallback(func(ctx context.Context, e *irc.Event) {
		server := ctx.Value("server").(*Server)
		if len(e.Arguments) == 4 {
			for _, nick := range strings.Split(e.Arguments[3], " ") {
				server.GetChan(e.Arguments[2]).AddUser(nick)
			}
		}
		fmt.Printf("Added channel: %s -> %v", e.Arguments[2], server.GetChan(e.Arguments[0]))
	}))

	connection.AddCallback("PRIVMSG", ctxCallback(func(ctx context.Context, e *irc.Event) {
		msg := strings.TrimSpace(e.Message())
		if strings.HasPrefix(msg, "++") || strings.HasSuffix(msg, "++") {
			addKarma(e, strings.Trim(msg, "+ "), ctx)
			return
		}
		if strings.HasPrefix(msg, "--") || strings.HasSuffix(msg, "--") {
			subKarma(e, strings.Trim(msg, "- "), ctx)
			return
		}
		if e.Nick == "vfeenstr" || e.Nick == "evilissimo" && len(e.Arguments) == 1 {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(e.Message())), "quit") {
				if len(strings.TrimSpace(e.Message())) > 4 {
					e.Connection.QuitMessage = strings.TrimSpace(strings.TrimSpace(e.Message())[4:])
					fmt.Printf("Setting new QuitMessage to `%s`\n", e.Connection.QuitMessage)
				}
				e.Connection.Quit()
			} else {
				e.Connection.SendRaw(e.Message())
			}
		}
		if strings.HasPrefix(msg, e.Connection.Nick+":") {
			// TODO: More commands
		}
	}))

	connection.VerboseCallbackHandler = true
	connection.Debug = true
	err = connection.Connect("irc.eng.brq.redhat.com:6667")
	if err == nil {
		connection.Loop()
	} else {
		fmt.Printf("Failed to connect: %s \n", err.Error())
	}
}
