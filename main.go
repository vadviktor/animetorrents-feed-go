package main

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

const (
	username           = "vadviktor"
	password           = "rbT6uUuZDVYPb3SF"
	loginUrl           = "https://animetorrents.me/login.php"
	torrentsUrl        = "https://animetorrents.me/torrents.php"
	torrentListUrl     = "https://animetorrents.me/ajax/torrents_data.php?total=%d&page=%d"
	outputFilename     = "atom.xml"
	torrentPagesToScan = 10
)

type animerTorrents struct {
	client          *http.Client
	maxTorrentPages int
}

var regexpImgTag = regexp.MustCompile(`<img.+/>`)
var regexpExcludedCategories = regexp.MustCompile(`(Manga|Novel|Doujin)`)
var regexpPlot = regexp.MustCompile(`<div id="torDescription">[\s\w\W]*</div>`)
var regexpCoverImage = regexp.MustCompile(`<img src="https://animetorrents\.me/imghost/covers/.*/>`)
var regexpScreenShots = regexp.MustCompile(`<img src="https://animetorrents\.me/imghost/screenthumb/.*/>`)

func main() {
	now := time.Now()

	feed := &Feed{
		Updated:     now.Format(time.RFC3339),
		Link:        "http://vadviktor.xyz/rss/animetorrents/rss.xml",
		Description: "Extracted torrent information for Animetorrents.me",
		Author: Person{
			Name:  "Viktor (Ikon) VAD",
			Email: "vad.viktor@gmail.com",
			URI:   "https://github.com/vadviktor",
		},
		Title: "Animetorrents.me feed",
	}

	a := &animerTorrents{}
	a.create()
	a.login()
	a.maxPages()

	regexpTorrentRows := regexp.MustCompile(`(?mU)<tr class="data(Odd|Even)[\s\S]*<a[\s\w\W]+title="(?P<category>.+)"[\s\S]+<a href="(?P<url>.+)"[\s\S]+<strong>(?P<title>.+)</a>`)

	log.Println("Start to parse torrent list pages.")
	for i := 1; i <= torrentPagesToScan; i++ {
		log.Println("Take 3 seconds break not to hammer the server.")
		time.Sleep(3 * time.Second)

		body := a.listPageResponse(i)

		// parse items, select which to get the profile for
		namedGroups := regexpTorrentRows.SubexpNames()
		matches := regexpTorrentRows.FindAllStringSubmatch(body, -1)
		results := make(map[string]string)
		for _, match := range matches {
			for j, namedMatch := range match {
				results[namedGroups[j]] = namedMatch
			}

			// Skip unwanted categories.
			if regexpExcludedCategories.MatchString(results["category"]) == true {
				continue
			}

			feedItem := &Entry{
				Title:    cleanTitle(results["title"]),
				Link:     results["url"],
				Category: results["category"],
			}

			// get the profile for each selected item
			log.Println("Take 3 seconds break not to hammer the server.")
			time.Sleep(3 * time.Second)
			log.Printf("Reading torrent profile: %s\n", results["url"])
			a.fillTorrentProfileContent(feedItem, results["url"])

			feed.Entry = append(feed.Entry, feedItem)
		}
	}

	// write out rss file
	err := ioutil.WriteFile(outputFilename, feed.Build(), 0644)
	if err != nil {
		log.Fatalf("Failed to write to output file: %s\n", err.Error())
	}
}

func (a *animerTorrents) fillTorrentProfileContent(feedItem *Entry, torrentProfileUrl string) {
	resp, err := a.client.Get(torrentProfileUrl)
	defer resp.Body.Close()
	if err != nil {
		log.Fatalf("Failed to get the torrent profile page: %s\n", err.Error())
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read torrent profile response body: %s\n", err.Error())
	}

	plotMatch := regexpPlot.FindString(string(body))
	coverImageMatch := regexpCoverImage.FindString(string(body))
	screenshotsMatch := regexpScreenShots.FindString(string(body))

	feedItem.Content = html.EscapeString(fmt.Sprintf("%s\n%s\n%s\n",
		plotMatch, coverImageMatch, screenshotsMatch))
}

func (a *animerTorrents) listPageResponse(pageNumber int) string {
	log.Printf("Getting list page no.: %d\n", pageNumber)

	var buf io.Reader
	req, err := http.NewRequest("GET",
		fmt.Sprintf(torrentListUrl, a.maxTorrentPages, pageNumber), buf)
	if err != nil {
		log.Fatalf("Failed creating new request for page no. %s\n%s\n",
			pageNumber, err.Error())
	}
	req.Header.Add("X-Requested-With", "XMLHttpRequest")

	resp, err := a.client.Do(req)
	if err != nil {
		log.Fatalf("Failed to GET the page: %s\n%s\n", pageNumber,
			err.Error())
	}
	defer resp.Body.Close()

	log.Printf("Response status code: %d\n", resp.StatusCode)
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read torrent page response body: %s\n",
			err.Error())
	}

	log.Printf("Return body length: %d", len(body))

	if strings.Contains(string(body), "Access Denied!") {
		log.Fatalf("Failed to access torrent page %d", pageNumber)
	}

	return string(body)
}

func (a *animerTorrents) create() {
	log.Println("Creating http client.")
	options := cookiejar.Options{PublicSuffixList: publicsuffix.List}
	jar, err := cookiejar.New(&options)
	if err != nil {
		log.Fatal(err)
	}

	a.client = &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second}
}

func (a *animerTorrents) login() {
	log.Println("Logging in.")
	if _, err := a.client.Get(loginUrl); err != nil {
		log.Fatalf("Failed to get login page: %s\n", err.Error())
	}

	params := url.Values{}
	params.Add("form", "login")
	params.Add("username", username)
	params.Add("password", password)
	resp, err := a.client.PostForm(loginUrl, params)
	defer resp.Body.Close()
	if err != nil {
		log.Fatalf("Failed to post login data: %s\n", err.Error())
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read login response body: %s\n", err.Error())
	}

	// A known error text upon failed login.
	if strings.Contains(string(body),
		"Error: Invalid username or password.") {
		log.Fatalln("Login failed: invalid username or password.")
	}

	// If I can't see my username, then I am not logged in.
	if !strings.Contains(string(body), username) {
		log.Fatalln("Login failed: can't find username in response body.")
	}

	log.Println("Logged in.")
}

func (a *animerTorrents) maxPages() {
	log.Println("Finding out torrents max page number.")
	resp, err := a.client.Get(torrentsUrl)
	defer resp.Body.Close()
	if err != nil {
		log.Fatalf("Failed to get the torrents page: %s\n", err.Error())
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read torrents list response body: %s\n", err.Error())
	}

	re := regexp.MustCompile(`ajax/torrents_data\.php\?total=(\d+)&page=1`)
	match := re.FindStringSubmatch(string(body))
	if len(match) > 1 {
		total, err := strconv.Atoi(match[1])
		if err != nil {
			log.Fatalf("Can't convert %s to int.", total)
		}
		log.Printf("Max pages figured out: %d.\n", total)
		a.maxTorrentPages = total
	}
}

func cleanTitle(dirtyTitle string) (cleanTitle string) {
	cleanTitle = strings.Replace(dirtyTitle, "</strong>", "", -1)
	cleanTitle = regexpImgTag.ReplaceAllLiteralString(cleanTitle, "")
	cleanTitle = html.EscapeString(cleanTitle)
	return
}

type Feed struct {
	Author      Person
	Title       string
	Updated     string
	Description string
	Link        string

	// Entries
	Entry []*Entry
}

type Entry struct {
	Title    string
	Link     string
	Category string
	Content  string
}

type Person struct {
	Name  string
	URI   string
	Email string
}

// Build function will put the feed data together into an Atom feed structure.
func (f *Feed) Build() []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version='1.0' encoding='UTF-8'?>
<rss xmlns:atom="http://www.w3.org/2005/Atom" xmlns:content="http://purl.org/rss/1.0/modules/content/" version="2.0"><channel>`)
	b.WriteString(fmt.Sprintf(`<title>%s</title>`, f.Title))
	b.WriteString(fmt.Sprintf(`<lastBuildDate>%s</lastBuildDate>`, f.Updated))
	b.WriteString(fmt.Sprintf(`<atom:link href="%s" rel="self"/>`, f.Link))
	b.WriteString(fmt.Sprintf(`<description>%s</description>`, f.Description))
	b.WriteString(fmt.Sprintf(`<generator>Go 1.9</generator>`))

	b.WriteString(`<author>`)
	b.WriteString(fmt.Sprintf(`<name>%s</name>`, f.Author.Name))
	b.WriteString(fmt.Sprintf(`<uri>%s</uri>`, f.Author.URI))
	b.WriteString(fmt.Sprintf(`<email>%s</email>`, f.Author.Email))
	b.WriteString(`</author>`)

	for _, e := range f.Entry {
		b.WriteString(fmt.Sprintf(`<item>`))
		b.WriteString(fmt.Sprintf(`<title>%s</title>`, e.Title))
		b.WriteString(fmt.Sprintf(`<link>%s</link>`, e.Link))
		b.WriteString(fmt.Sprintf(`<guid isPermaLink="false">%s</guid>`,
			e.Link))
		b.WriteString(fmt.Sprintf(`<category>%s</category>`, e.Category))
		b.WriteString(fmt.Sprintf(`<description>%s</description>`,
			e.Content))
		b.WriteString(fmt.Sprintf(`</item>`))
	}

	b.WriteString(`</channel></rss>`)

	return b.Bytes()
}
