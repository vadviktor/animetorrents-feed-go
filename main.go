package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/sethgrid/pester"
	"golang.org/x/net/publicsuffix"
)

const (
	loginUsername      = "vadviktor"
	loginPassword      = "rbT6uUuZDVYPb3SF"
	loginURL           = "https://animetorrents.me/login.php"
	torrentsURL        = "https://animetorrents.me/torrents.php"
	torrentListURL     = "https://animetorrents.me/ajax/torrents_data.php?total=%d&page=%d"
	torrentPagesToScan = 3
	antiHammerMaxSleep = 5
	slackWebhookURL    = "https://hooks.slack.com/services/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"
	doSpacesKey        = "Y5MQIQZMVDWWC7JW3IFR"
	doSpacesSecret     = "LwP0cyWHaOejGRzacNHnpZfb/mxsOynvFVEVnfYEOvY"
	doSpacesEndpoint   = "https://ams3.digitaloceanspaces.com"
	doSpacesRegion     = "eu-west-1"
	doSpacesBucket     = "ikon"
	doSpacesObjectName = "animetorrents-feed.xml"
)

type animeTorrents struct {
	client          *pester.Client
	maxTorrentPages int
	slack           *slack
}

type atomFeed struct {
	Author  feedPerson
	Title   string
	Updated string
	Link    string
	Entry   []*feedEntry
}

type feedEntry struct {
	Updated  string
	Title    string
	Link     string
	Category string
	Content  string
}

type feedPerson struct {
	Name  string
	URI   string
	Email string
}

type slack struct {
	client *http.Client
}

var (
	regexpTorrentRows        = regexp.MustCompile(`(?mU)<tr class="data(Odd|Even)[\s\S]*<a[\s\w\W]+title="(?P<category>.+)"[\s\S]+<a href="(?P<url>.+)"[\s\S]+<strong>(?P<title>.+)</a>`)
	regexpImgTag             = regexp.MustCompile(`<img.+/>`)
	regexpExcludedCategories = regexp.MustCompile(`(Manga|Novel|Doujin)`)
	regexpPlot               = regexp.MustCompile(`<div id="torDescription">[\s\w\W]*</div>`)
	regexpCoverImage         = regexp.MustCompile(`<img src="https://animetorrents\.me/imghost/covers/.*/>`)
	regexpScreenShots        = regexp.MustCompile(`<img src="https://animetorrents\.me/imghost/screenthumb/.*/>`)
	regexpEntryUpdated       = regexp.MustCompile(`<span class="blogDate">(.*])</span>`)
)

func main() {
	s := &slack{}
	s.create()
	s.send("Begin to crawl.")

	if len(os.Args) < 2 {
		s.send("Not enough parameters, missing output file path!")
		log.Fatalln("Not enough parameters, missing output file path!")
	}

	// Parse HTML template once.
	entryContentTemplate, err := template.New("content").Parse(`
		<div>{{.CoverImage}}</div>
		<div>[{{.Category}}]</div>
		<div>{{.AbsoluteLink}}</div>
		{{.Plot}}
		<div>{{.Screenshots}}</div>
	`)
	if err != nil {
		log.Fatalf("Failed to parse template: %s\n", err.Error())
	}

	feed := &atomFeed{
		Updated: time.Now().Format(time.RFC3339),
		Link: fmt.Sprintf("https://s3-%s.amazonaws.com/%s/%s",
			doSpacesRegion, doSpacesBucket, doSpacesObjectName),
		Author: feedPerson{
			Name:  "Viktor (Ikon) VAD",
			Email: "vad.viktor@gmail.com",
			URI:   "https://www.github.com/vadviktor",
		},
		Title: "Animetorrents.me feed",
	}

	a := &animeTorrents{}
	a.create()
	a.slack = s
	a.login()
	a.maxPages()

	log.Println("Start to parse torrent list pages.")
	for i := 1; i <= torrentPagesToScan; i++ {
		time.Sleep(random(1, antiHammerMaxSleep) * time.Second)

		body := a.listPageResponse(i)

		// Parse items, select which to get the profile for.
		namedGroups := regexpTorrentRows.SubexpNames()
		matches := regexpTorrentRows.FindAllStringSubmatch(body, -1)
		results := make(map[string]string)
		for _, match := range matches {
			for j, namedMatch := range match {
				results[namedGroups[j]] = namedMatch
			}

			// Skip unwanted categories.
			if regexpExcludedCategories.MatchString(results["category"]) {
				continue
			}

			feedItem := &feedEntry{
				Title:    cleanTitle(results["title"]),
				Link:     results["url"],
				Category: results["category"],
			}

			// Get the profile for each selected item.
			time.Sleep(random(1, antiHammerMaxSleep) * time.Second)
			log.Printf("Reading torrent profile: %s\n", results["url"])
			a.parseProfile(feedItem, results, entryContentTemplate)

			feed.Entry = append(feed.Entry, feedItem)
		}
	}

	// Write the rss file to disk.
	err = ioutil.WriteFile(os.Args[1], feed.Build(), 0644)
	if err != nil {
		s.send("Failed to write to output file: %s\n", err.Error())
		log.Fatalf("Failed to write to output file: %s\n", err.Error())
	}
	defer os.Remove(os.Args[1])

	err = putOnS3(os.Args[1])
	if err != nil {
		s.send("Failure during uploading file to S3: %s\n", err.Error())
		log.Fatalf("Failure during uploading file to S3: %s\n", err.Error())
	}

	s.send("Atom feed is ready.")
	log.Println("Script finished.")
}

// parseProfile extracts the content from the torrent profile and fills in the
// entry fields.
func (a *animeTorrents) parseProfile(feedItem *feedEntry, torrentRowInfo map[string]string,
	tpl *template.Template) {
	resp, err := a.client.Get(torrentRowInfo["url"])
	if err != nil {
		a.slack.send("Failed to get the torrent profile page: %s\n", err.Error())
		log.Fatalf("Failed to get the torrent profile page: %s\n", err.Error())
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		a.slack.send("Failed to read torrent profile response body: %s\n", err.Error())
		log.Fatalf("Failed to read torrent profile response body: %s\n", err.Error())
	}
	bodyText := string(body)

	// Content.
	plotMatch := regexpPlot.FindString(bodyText)
	coverImageMatch := regexpCoverImage.FindString(bodyText)
	screenshotsMatch := regexpScreenShots.FindString(bodyText)

	data := struct {
		CoverImage   string
		Category     string
		AbsoluteLink string
		Plot         string
		Screenshots  string
	}{
		CoverImage:   coverImageMatch,
		Category:     torrentRowInfo["category"],
		AbsoluteLink: torrentRowInfo["url"],
		Plot:         plotMatch,
		Screenshots:  screenshotsMatch,
	}

	contentFromTpl := new(bytes.Buffer)
	err = tpl.Execute(contentFromTpl, data)
	if err != nil {
		log.Fatalf("Failed to generate output from template: %s\n", err.Error())
	}
	feedItem.Content = contentFromTpl.String()

	// Updated.
	updatedMatch := regexpEntryUpdated.FindStringSubmatch(bodyText)
	if len(updatedMatch) > 1 && updatedMatch[1] != "" {
		blogForm := "2 Jan, 2006 [3:04 pm]"
		t, err := time.Parse(blogForm, updatedMatch[1])
		if err != nil {
			a.slack.send("Unable to parse time format: %s\n", err.Error())
			feedItem.Updated = time.Now().Format(time.RFC3339)
		} else {
			feedItem.Updated = t.Format(time.RFC3339)
		}
	} else {
		a.slack.send("Unable to extract upload time data")
		feedItem.Updated = time.Now().Format(time.RFC3339)
	}
}

func (a *animeTorrents) listPageResponse(pageNumber int) string {
	log.Printf("Getting list page no.: %d\n", pageNumber)

	var buf io.Reader
	req, err := http.NewRequest("GET",
		fmt.Sprintf(torrentListURL, a.maxTorrentPages, pageNumber), buf)
	if err != nil {
		a.slack.send("Failed creating new request for page no. %d\n%s\n",
			pageNumber, err.Error())
		log.Fatalf("Failed creating new request for page no. %d\n%s\n",
			pageNumber, err.Error())
	}
	req.Header.Add("X-Requested-With", "XMLHttpRequest")

	resp, err := a.client.Do(req)
	if err != nil {
		a.slack.send("Failed to GET the page: %d\n%s\n", pageNumber,
			err.Error())
		log.Fatalf("Failed to GET the page: %d\n%s\n", pageNumber,
			err.Error())
	}
	defer resp.Body.Close()

	log.Printf("Response status code: %d\n", resp.StatusCode)
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		a.slack.send("Failed to read torrent page response body: %s\n",
			err.Error())
		log.Fatalf("Failed to read torrent page response body: %s\n",
			err.Error())
	}

	log.Printf("Return body length: %d", len(body))

	if strings.Contains(string(body), "Access Denied!") {
		a.slack.send("Failed to access torrent page %d", pageNumber)
		log.Fatalf("Failed to access torrent page %d", pageNumber)
	}

	return string(body)
}

func (a *animeTorrents) create() {
	log.Println("Creating http client.")
	options := cookiejar.Options{PublicSuffixList: publicsuffix.List}
	jar, err := cookiejar.New(&options)
	if err != nil {
		a.slack.send(err.Error())
		log.Fatal(err)
	}

	a.client = pester.New()
	a.client.Jar = jar
	a.client.Timeout = 30 * time.Second
	a.client.MaxRetries = 5
}

func (a *animeTorrents) login() {
	log.Println("Logging in.")
	if _, err := a.client.Get(loginURL); err != nil {
		a.slack.send("Failed to get login page: %s\n", err.Error())
		log.Fatalf("Failed to get login page: %s\n", err.Error())
	}

	params := url.Values{}
	params.Add("form", "login")
	params.Add("username", loginUsername)
	params.Add("password", loginPassword)
	resp, err := a.client.PostForm(loginURL, params)
	if err != nil {
		a.slack.send("Failed to post login data: %s\n", err.Error())
		log.Fatalf("Failed to post login data: %s\n", err.Error())
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		a.slack.send("Failed to read login response body: %s\n", err.Error())
		log.Fatalf("Failed to read login response body: %s\n", err.Error())
	}

	// A known error text upon failed login.
	if strings.Contains(string(body),
		"Error: Invalid username or password.") {
		a.slack.send("Login failed: invalid username or password.")
		log.Fatalln("Login failed: invalid username or password.")
	}

	// If I can't see my username, then I am not logged in.
	if !strings.Contains(string(body), loginUsername) {
		a.slack.send("Login failed: can't find username in response body.")
		log.Fatalln("Login failed: can't find username in response body.")
	}

	log.Println("Logged in.")
}

func (a *animeTorrents) maxPages() {
	log.Println("Finding out torrents max page number.")
	resp, err := a.client.Get(torrentsURL)
	if err != nil {
		a.slack.send("Failed to get the torrents page: %s\n", err.Error())
		log.Fatalf("Failed to get the torrents page: %s\n", err.Error())
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		a.slack.send("Failed to read torrents list response body: %s\n", err.Error())
		log.Fatalf("Failed to read torrents list response body: %s\n", err.Error())
	}

	re := regexp.MustCompile(`ajax/torrents_data\.php\?total=(\d+)&page=1`)
	match := re.FindStringSubmatch(string(body))
	if len(match) > 1 && match[1] != "" {
		total, err := strconv.Atoi(match[1])
		if err != nil {
			a.slack.send("Can't convert %d to int.", total)
			log.Fatalf("Can't convert %d to int.", total)
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

// Build will put the feed data together into an Atom feed structure.
func (f *atomFeed) Build() []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version='1.0' encoding='UTF-8'?>
<feed xmlns="http://www.w3.org/2005/Atom">`)
	b.WriteString(fmt.Sprintf(`<title>%s</title>`, f.Title))
	b.WriteString(fmt.Sprintf(`<updated>%s</updated>`, f.Updated))
	b.WriteString(fmt.Sprintf(`<id>%s</id>`, f.Link))
	b.WriteString(fmt.Sprintf(`<link href="%s" rel="self" />`, f.Link))
	b.WriteString(fmt.Sprintf(`<generator>Go 1.9</generator>`))
	b.WriteString(`<author>`)
	b.WriteString(fmt.Sprintf(`<name>%s</name>`, f.Author.Name))
	b.WriteString(fmt.Sprintf(`<uri>%s</uri>`, f.Author.URI))
	b.WriteString(fmt.Sprintf(`<email>%s</email>`, f.Author.Email))
	b.WriteString(`</author>`)

	for _, e := range f.Entry {
		b.WriteString(fmt.Sprintf(`<entry>`))
		b.WriteString(fmt.Sprintf(`<title>%s</title>`, e.Title))
		b.WriteString(fmt.Sprintf(`<link href="%s" rel="self" />`, e.Link))
		b.WriteString(fmt.Sprintf(`<id>%s</id>`, e.Link))
		b.WriteString(fmt.Sprintf(`<updated>%s</updated>`, e.Updated))
		b.WriteString(fmt.Sprintf(`<content type="html">%s</content>`, e.Content))
		b.WriteString(fmt.Sprintf(`</entry>`))
	}

	b.WriteString(`</feed>`)

	return b.Bytes()
}

func (s *slack) create() {
	s.client = &http.Client{
		Timeout: 30 * time.Second,
	}
}

func (s *slack) send(text string, params ...interface{}) {
	t := map[string]string{"text": fmt.Sprintf(text, params...)}
	payload, err := json.Marshal(t)
	if err != nil {
		log.Fatalf("Failed to create json payload for Slack: %s\n",
			err.Error())
	}

	p := strings.NewReader(string(payload))
	resp, err := s.client.Post(slackWebhookURL, "application/json", p)
	if err != nil {
		log.Fatalf("Failed to pass text to Slack: %s\n", err.Error())
	}
	defer resp.Body.Close()
}

func putOnS3(filePath string) error {
	sess, err := session.NewSession(&aws.Config{
		Endpoint:    aws.String(doSpacesEndpoint),
		Region:      aws.String(doSpacesRegion),
		Credentials: credentials.NewStaticCredentials(doSpacesKey, doSpacesSecret, ""),
	})
	if err != nil {
		return err
	}

	// upload
	uploader := s3manager.NewUploader(sess)

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket:          aws.String(doSpacesBucket),
		Key:             aws.String(doSpacesObjectName),
		Body:            file,
		ACL:             aws.String("public-read"),
		ContentType:     aws.String("application/atom+xml"),
		ContentEncoding: aws.String("utf-8"),
	})

	return err
}

func random(min, max int) time.Duration {
	rand.Seed(time.Now().UnixNano())
	return time.Duration(rand.Intn(max-min) + min)
}
