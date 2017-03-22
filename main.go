package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

const intergatorTmpl = "https://search.kuehne-nagel.com/web/ig-kn/?q=%s&of=%d"

type multifetch struct {
	group cacheGroup
	q     string
	tmpl  string
	first int
	pages int
}

func newMultifetch(group cacheGroup, tmpl string, n, total int) *multifetch {
	return &multifetch{
		group: group,
		q:     url.QueryEscape(string(group)),
		tmpl:  tmpl,
		first: n,
		pages: total,
	}
}

func (m *multifetch) rangePages() (int, int) {
	return 0, m.pages
}

func (m *multifetch) fetch(c *cache, n int) {
	n = m.first + n
	url := fmt.Sprintf(m.tmpl, m.q, n*10) // TODO: offset should not be hardcoded
	log.Printf("debug: fetch request for %s", url)
	go func() {
		body, err := m.get(url)
		if err != nil {
			err = fmt.Errorf("cannot fetch URL %s: %s", url, err)
		}
		log.Printf("debug: fetch adds response to cache")
		c.add(m.group, newPage(n, body), err)
	}()
}

func (m *multifetch) get(url string) ([]byte, error) {
	tr := &http.Transport{
		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := &bytes.Buffer{}
	_, err = io.Copy(buf, resp.Body)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type page struct {
	n        int
	body     []byte
	expire time.Time
	cached   bool
}

func newPage(n int, body []byte) *page {
	return &page{
		n:    n,
		body: body,
	}
}

func (p *page) WriteTo(w io.Writer) (int, error) {
	n, err := w.Write(p.body)
	return n, err
}

type cacheGroup string

type cacheEntry struct {
	// deadline is applied to all pages because they are always accessed in order
	deadline time.Time
	pages    map[int][]byte
}

func newCacheEntry(d time.Duration) *cacheEntry {
	return &cacheEntry{
		deadline: time.Now().Add(d),
		pages:    make(map[int][]byte),
	}
}

func (ce *cacheEntry) invalid(t time.Time) bool {
	return !ce.deadline.After(t)
}

func (ce *cacheEntry) getPage(n int) (*page, bool) {
	b, ok := ce.pages[n]
	if !ok {
		return nil, false
	}
	p := newPage(n, b)
	p.expire = ce.deadline
	return p, ok
}

func (ce *cacheEntry) addPage(p *page) {
	// TODO: I might want to bump the deadline for this entry
	ce.pages[p.n] = p.body
}

type waiters struct {
	waits map[cacheGroup]map[int]chan struct{}
}

func newWaiters() *waiters {
	return &waiters{
		waits: make(map[cacheGroup]map[int]chan struct{}),
	}
}

func (w *waiters) wait(group cacheGroup, n int) chan struct{} {
	if _, ok := w.waits[group]; !ok {
		w.waits[group] = make(map[int]chan struct{})
	} else {
		if ch, ok := w.waits[group][n]; ok {
			return ch
		}
	}
	ch := make(chan struct{})
	w.waits[group][n] = ch
	return ch
}

func (w *waiters) done(group cacheGroup, n int) {
	_, ok := w.waits[group]
	if !ok {
		return
	}
	ch, ok := w.waits[group][n]
	if !ok {
		return
	}
	close(ch)
	delete(w.waits[group], n)
	// Cleanup
	if len(w.waits[group]) == 0 {
		delete(w.waits, group)
	}
}

type cacheFunc func() error

type cache struct {
	entries map[cacheGroup]*cacheEntry
	waits   *waiters
	events  chan cacheFunc
}

func newCache() *cache {
	c := &cache{
		entries: make(map[cacheGroup]*cacheEntry),
		events:  make(chan cacheFunc),
		waits:   newWaiters(),
	}
	go c.run()
	go c.gc(5 * time.Second) // TODO: not hardcoded
	return c
}

func (c *cache) run() {
	for f := range c.events {
		if err := f(); err != nil {
			log.Print("cache: ", err)
		}
	}
}

func (c *cache) gc(d time.Duration) {
	done := make(chan struct{})
	for {
		time.Sleep(d)
		c.events <- func() error {
			var purge []cacheGroup
			now := time.Now()
			for cgroup, entry := range c.entries {
				if entry.invalid(now) {
					purge = append(purge, cgroup)
				}
			}
			if len(purge) > 0 {
				for _, cgroup := range purge {
					delete(c.entries, cgroup)
				}
			}
			done <- struct{}{}
			return nil
		}
		<-done
	}
}

// add inserts a page into the cache (after it was fetched).
func (c *cache) add(group cacheGroup, p *page, err error) {
	c.events <- func() error {
		ce, ok := c.entries[group]
		if !ok {
			ce = newCacheEntry(5 * time.Minute)
		}
		ce.addPage(p)
		c.entries[group] = ce
		log.Printf("debug: added page %s/%d", group, p.n)
		// If there were waiters, signal that the wait is over
		c.waits.done(group, p.n)
		return err
	}
}

func (c *cache) request(group cacheGroup, n int) chan struct{} {
	wait := c.waits.wait(group, n)
	mf := newMultifetch(group, intergatorTmpl, n, 3)
	mf.fetch(c, n)
	lo, hi := mf.rangePages()
	for ; lo < hi; lo++ {
		c.waits.wait(group, lo)
		mf.fetch(c, lo)
	}
	return wait
}

func (c *cache) get(group cacheGroup, n int) *page {
	var (
		page *page
		wait chan struct{}
	)
	cached := true
	requested := make(chan struct{})
	for {
		wait = nil
		c.events <- func() error {
			defer func() { requested <- struct{}{} }()
			var now time.Time
			ce, ok := c.entries[group]
			if !ok {
				now = time.Now()
			}
			if !ok || ce.invalid(now) {
				log.Printf("debug: %s/%d: not cached, requested", group, n)
				wait = c.request(group, n)
				return nil
			}
			page, ok = ce.getPage(n)
			if !ok {
				log.Printf("debug: %s/%d: not cached, requested", group, n)
				wait = c.request(group, n)
				return nil
			}
			return nil
		}
		<-requested
		// content was already in cache, return it
		if wait == nil {
			log.Printf("debug: %s/%d: found", group, n)
			page.cached = cached
			return page
		}
		// We needed to request the object, it was not cached
		cached = false
		log.Printf("debug: %s/%d: waiting", group, n)
		// content is being fetched, wait and try to get again
		<-wait
	}
	// unreacheable
	return nil
}

type origin struct {
	name  string
	cache *cache
}

func newOrigin(name string) *origin {
	return &origin{
		name:  name,
		cache: newCache(),
	}
}

func (o *origin) handle(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	q := vars["q"]
	if q == "" {
		http.NotFound(w, r)
		return
	}
	n := 0
	if vars["n"] != "" {
		m, err := strconv.ParseInt(vars["n"], 10, 64)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		n = int(m)
	}
	log.Printf("debug: %s/%d: requesting from cache", q, n)
	// TODO: there should be some sort of timeout here
	page := o.cache.get(cacheGroup(q), n)
	if page.cached {
		w.Header().Set("X-From-Cache", "1");
	}
	w.Header().Set("X-Cached-Until", page.expire.Format(time.RFC3339))
	if _, err := page.WriteTo(w); err != nil {
		log.Printf("http: error writing response body: %s", err)
	}
}

type origins struct {
	o map[string]*origin
}

func newOrigins() *origins {
	return &origins{
		o: make(map[string]*origin),
	}
}

func (ors *origins) add(o *origin) {
	ors.o[o.name] = o
}

func (ors *origins) initRouter(r *mux.Router) {
	for k := range ors.o {
		r.HandleFunc(fmt.Sprintf("/%s/search/{q}", ors.o[k].name), ors.o[k].handle)
		r.HandleFunc(fmt.Sprintf("/%s/search/{q}/{n}", ors.o[k].name), ors.o[k].handle)
	}
}

func main() {
	origins := newOrigins()
	origins.add(newOrigin("intergator"))

	r := mux.NewRouter()
	origins.initRouter(r)

	http.Handle("/", r)
	log.Fatal(http.ListenAndServe(":8383", nil))
}
