package wwcp

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"text/template"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/taskqueue"
	"appengine/user"

	"github.com/mjibson/appstats"
)

const maxBytes = 2 << 20

func init() {
	appstats.RecordFraction = 1

	http.Handle("/", appstats.NewHandler(handleIndex))
	http.Handle("/feeds/", appstats.NewHandler(handleListFeeds))
	http.Handle("/feeds/new", appstats.NewHandler(handleNewFeed))

	http.Handle("/q/push/", appstats.NewHandler(handlePush))
	http.Handle("/q/pull/", appstats.NewHandler(handlePull))
	http.Handle("/q/rm/", appstats.NewHandler(handleComplete))
}

type Feed struct {
	Owner string
	Name  string

	Key *datastore.Key `datastore:"-"`
}

type Message struct {
	Created     time.Time   `json:"created"`
	RemoteAddr  string      `json:"remote_addr"`
	QueryString string      `json:"query"`
	Header      http.Header `json:"header"`
	Body        []byte      `json:"body"`
}

var templates = template.Must(template.New("").ParseGlob("templates/*.html"))

func handleIndex(c appengine.Context, w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "index.html", nil)
}

func reportError(c appengine.Context, w http.ResponseWriter, err error) {
	c.Warningf("Error: %v", err)
	http.Error(w, "Error processing your request", 500)
}

func handleListFeeds(c appengine.Context, w http.ResponseWriter, r *http.Request) {
	q := datastore.NewQuery("Feed").Filter("Owner = ", user.Current(c).Email)
	feeds := []Feed{}
	keys, err := q.GetAll(c, &feeds)
	if err != nil {
		reportError(c, w, err)
		return
	}

	for i := range feeds {
		feeds[i].Key = keys[i]
	}

	templates.ExecuteTemplate(w, "feeds.html", feeds)
}

func handleNewFeed(c appengine.Context, w http.ResponseWriter, r *http.Request) {
	feed := &Feed{
		Owner: user.Current(c).Email,
		Name:  r.FormValue("name"),
	}

	_, err := datastore.Put(c, datastore.NewIncompleteKey(c, "Feed", nil), feed)
	if err != nil {
		reportError(c, w, err)
		return
	}

	http.Redirect(w, r, "/feeds/", http.StatusFound)
}

type cachedFeed struct {
	feed *Feed
	ttl  time.Time
}

func (c *cachedFeed) Feed() *Feed {
	if c == nil {
		return nil
	}
	return c.feed
}

type feedCacheT struct {
	m  map[string]*cachedFeed
	mu sync.Mutex
}

func (f *feedCacheT) Get(name string) (*Feed, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cf, ok := f.m[name]
	if cf != nil && cf.ttl.Before(time.Now()) {
		delete(f.m, name)
		cf.feed = nil
		ok = false
	}
	return cf.Feed(), ok
}

func (f *feedCacheT) Set(name string, feed *Feed) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.m[name] = &cachedFeed{feed: feed, ttl: time.Now().Add(time.Hour)}
}

var (
	feedCache = &feedCacheT{m: map[string]*cachedFeed{}}
	errNoFeed = errors.New("no such feed")
)

func getFeed(c appengine.Context, kstr string) (*Feed, error) {
	feed, ok := feedCache.Get(kstr)
	if ok {
		if feed == nil {
			return nil, errNoFeed
		}
		return feed, nil
	}

	k, err := datastore.DecodeKey(kstr)
	if err != nil {
		return nil, err
	}

	feed = &Feed{}
	err = datastore.Get(c, k, feed)
	feed.Key = k
	if err == nil {
		feedCache.Set(kstr, feed)
	} else {
		feedCache.Set(kstr, nil)
	}
	return feed, err
}

func handlePush(c appengine.Context, w http.ResponseWriter, r *http.Request) {
	kstr := r.URL.Path[8:]
	_, err := getFeed(c, kstr)
	if err != nil {
		reportError(c, w, err)
		return
	}

	body, err := ioutil.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		reportError(c, w, err)
		return
	}

	msg := &Message{
		Created:     time.Now().UTC(),
		RemoteAddr:  r.RemoteAddr,
		Header:      r.Header,
		QueryString: r.URL.Query().Encode(),
		Body:        body,
	}

	buf := &bytes.Buffer{}
	genc := gob.NewEncoder(buf)
	if err := genc.Encode(msg); err != nil {
		reportError(c, w, err)
		return
	}

	if _, err := taskqueue.Add(c, &taskqueue.Task{
		Method:  "PULL",
		Payload: buf.Bytes(),
		Tag:     kstr,
	}, "todo"); err != nil {
		reportError(c, w, err)
		return
	}

	w.WriteHeader(201)
}

func handlePull(c appengine.Context, w http.ResponseWriter, r *http.Request) {
	kstr := r.URL.Path[8:]
	tasks, err := taskqueue.LeaseByTag(c, 1, "todo", 30, kstr)
	if err != nil {
		reportError(c, w, err)
		return
	}
	if len(tasks) != 1 {
		c.Infof("No tasks found")
		w.WriteHeader(204)
		return
	}

	task := tasks[0]

	buf := bytes.NewReader(task.Payload)
	gdec := gob.NewDecoder(buf)
	msg := &Message{}
	if err := gdec.Decode(msg); err != nil {
		reportError(c, w, err)
		return
	}

	j := json.NewEncoder(w)
	err = j.Encode(map[string]interface{}{
		"tid":         task.Name,
		"headers":     msg.Header,
		"created":     msg.Created,
		"query":       msg.QueryString,
		"remote_addr": msg.RemoteAddr,
		"body":        msg.Body,
	})
	if err != nil {
		reportError(c, w, err)
		return
	}
}

func handleComplete(c appengine.Context, w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(r.URL.Path[6:], "/", 2)
	if len(parts) != 2 {
		http.Error(w, "you're doing it wrong", 400)
		return
	}
	_, err := getFeed(c, parts[0])
	if err != nil {
		reportError(c, w, err)
		return
	}
	if err := taskqueue.Delete(c, &taskqueue.Task{Name: parts[1]}, "todo"); err != nil {
		reportError(c, w, err)
		return
	}

	w.WriteHeader(204)
}
