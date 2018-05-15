package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/getsentry/raven-go"
	"github.com/sethgrid/pester"
	"github.com/spf13/viper"
	"github.com/vadviktor/lockfile"
	"github.com/vadviktor/telegram-msg"
	"golang.org/x/net/publicsuffix"
)

const (
	loginURL       = "https://animetorrents.me/login.php"
	torrentsURL    = "https://animetorrents.me/torrents.php"
	torrentListURL = "https://animetorrents.me/ajax/torrents_data.php?total=%d&page=%d"
)

// animeTorrents is the crawler itself.
type animeTorrents struct {
	client          *pester.Client
	maxTorrentPages int
	telegram        *telegram_msg.Telegram
}

// atomFeed is the main body of the Atom feed structure.
type atomFeed struct {
	Author  feedPerson
	Title   string
	Updated string
	Link    string
	Entry   []*feedEntry
}

// feedEntry represents a single article in a feed.
type feedEntry struct {
	Updated  string
	Title    string
	Link     string
	Category string
	Content  string
}

// feedPerson is
type feedPerson struct {
	Name  string
	URI   string
	Email string
}

type Error struct {
	msg string
}

func (e *Error) Error() string { return e.msg }

var (
	telegram                 = &telegram_msg.Telegram{}
	regexpTorrentRows        = regexp.MustCompile(`(?mU)<tr class="data(Odd|Even)[\s\S]*<a[\s\w\W]+title="(?P<category>.+)"[\s\S]+<a href="(?P<url>.+)"[\s\S]+<strong>(?P<title>.+)</a>`)
	regexpImgTag             = regexp.MustCompile(`<img.+/>`)
	regexpExcludedCategories = regexp.MustCompile(`(Manga|Novel|Doujin)`)
	regexpPlot               = regexp.MustCompile(`<div id="torDescription">[\s\w\W]*</div>`)
	regexpCoverImage         = regexp.MustCompile(`<img src="https://animetorrents\.me/imghost/covers/.*/>`)
	regexpScreenShots        = regexp.MustCompile(`<img src="https://animetorrents\.me/imghost/screenthumb/.*/>`)
	regexpEntryUpdated       = regexp.MustCompile(`<span class="blogDate">(.*])</span>`)
	fileBaseName             string
)

func init() {
	fileBaseName = strings.TrimRight(filepath.Base(os.Args[0]),
		filepath.Ext(os.Args[0]))

	flag.Usage = func() {
		u := fmt.Sprintf(`Logs into Animetorrents.me and gets the last 3 pages of torrents,
extracts their data and structures them in an Atom feed.
That Atom feed is then uploaded to DigitalOcean Spaces.

Create a config file named %s.json by filling in what is defined in its sample file.
`, fileBaseName)
		fmt.Fprint(os.Stderr, u)
	}
	flag.Parse()

	viper.SetConfigName(fileBaseName)
	viper.AddConfigPath(".")
	err := viper.ReadInConfig()
	if err != nil {
		log.Fatalf("%s\n", err.Error())
	}

	raven.SetDSN(viper.GetString("sentryDns"))
}

func main() {
	telegram.Create(viper.GetString("botToken"), viper.GetInt("targetId"))

	// Locking.
	lockFile, err := lockfile.Lock()
	if err != nil {
		raven.CaptureErrorAndWait(err, nil)
		telegram.Send(err.Error())
		log.Fatalf("%s\n", err.Error())
	}
	defer os.Remove(lockFile)

	//telegram.SendSilent("Begin to crawl.")

	// Parse HTML template once.
	entryContentTemplate, err := template.New("content").Parse(`
		{{.CoverImage}}
		<p>[{{.Category}}]</p>
		<p><a href="{{.AbsoluteLink}}" target="blank">{{.AbsoluteLink}}</a></p>
		{{.Plot}}
		{{.Screenshots}}
	`)
	if err != nil {
		raven.CaptureErrorAndWait(err, nil)
		log.Printf("Failed to parse template: %s\n", err.Error())
		telegram.Send(err.Error())
		return
	}

	feed := &atomFeed{
		Updated: time.Now().Format(time.RFC3339),
		Link: fmt.Sprintf("https://s3-%s.amazonaws.com/%s/%s",
			viper.GetString("doSpacesRegion"),
			viper.GetString("doSpacesBucket"),
			viper.GetString("doSpacesObjectName")),
		Author: feedPerson{
			Name:  "Viktor (Ikon) VAD",
			Email: "vad.viktor@gmail.com",
			URI:   "https://www.github.com/vadviktor",
		},
		Title: "Animetorrents.me feed",
	}

	a := &animeTorrents{}
	err = a.create()
	if err != nil {
		log.Printf("%s\n", err.Error())
		raven.CaptureErrorAndWait(err, nil)
		telegram.Send(err.Error())
		return
	}

	a.telegram = telegram
	err = a.login()
	if err != nil {
		raven.CaptureErrorAndWait(err, nil)
		log.Printf("%s\n", err.Error())
		telegram.Send(err.Error())
		return
	}

	err = a.maxPages()
	if err != nil {
		raven.CaptureErrorAndWait(err, nil)
		log.Printf("%s\n", err.Error())
		telegram.Send(err.Error())
		return
	}

	log.Println("Start to parse torrent list pages.")
	for i := 1; i <= viper.GetInt("torrentPagesToScan"); i++ {
		time.Sleep(random(1, viper.GetInt("antiHammerMaxSleep")) * time.Second)

		body, err := a.listPageResponse(i)
		if err != nil {
			raven.CaptureErrorAndWait(err, nil)
			log.Printf("%s\n", err.Error())
			telegram.Send(err.Error())
			return
		}

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
			time.Sleep(random(1, viper.GetInt("antiHammerMaxSleep")) * time.Second)
			log.Printf("Reading torrent profile: %s\n", results["url"])
			err := a.parseProfile(feedItem, results, entryContentTemplate)
			if err != nil {
				raven.CaptureErrorAndWait(err, nil)
				log.Printf("%s\n", err.Error())
				telegram.Send(err.Error())
				return
			}

			feed.Entry = append(feed.Entry, feedItem)
		}
	}

	// Write the rss file to disk.
	tempFeedFile := fmt.Sprintf("./%s.xml", fileBaseName)
	err = ioutil.WriteFile(tempFeedFile, feed.Build(), 0644)
	if err != nil {
		raven.CaptureErrorAndWait(err, nil)
		telegram.Send(fmt.Sprintf(
			"Failed to write to output file: %s", err.Error()))
		log.Printf("Failed to write to output file: %s\n", err.Error())

		return
	}
	defer os.Remove(tempFeedFile)

	err = putOnS3(tempFeedFile)
	if err != nil {
		raven.CaptureErrorAndWait(err, nil)
		telegram.Send(fmt.Sprintf("Failure during uploading file to S3: %s", err.Error()))
		log.Printf("Failure during uploading file to S3: %s\n", err.Error())

		return
	}

	telegram.SendSilent("Atom feed is ready.")
	log.Println("Script finished.")
}

// parseProfile extracts the content from the torrent profile and fills in the
// entry fields.
func (a *animeTorrents) parseProfile(feedItem *feedEntry, torrentRowInfo map[string]string,
	tpl *template.Template) error {
	resp, err := a.client.Get(torrentRowInfo["url"])
	if err != nil {
		return fmt.Errorf(
			"failed to get the torrent profile page: %s", err.Error())
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf(
			"failed to read torrent profile response body: %s",
			err.Error())
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
		return fmt.Errorf(
			"failed to generate output from template: %s",
			err.Error())
	}
	feedItem.Content = html.EscapeString(contentFromTpl.String())

	// Updated.
	updatedMatch := regexpEntryUpdated.FindStringSubmatch(bodyText)
	if len(updatedMatch) > 1 && updatedMatch[1] != "" {
		blogForm := "2 Jan, 2006 [3:04 pm]"
		t, err := time.Parse(blogForm, updatedMatch[1])
		if err != nil {
			a.telegram.Send(fmt.Sprintf("Unable to parse time format: %s", err.Error()))
			feedItem.Updated = time.Now().Format(time.RFC3339)
		} else {
			feedItem.Updated = t.Format(time.RFC3339)
		}
	} else {
		a.telegram.Send("Unable to extract upload time data")
		feedItem.Updated = time.Now().Format(time.RFC3339)
	}

	return nil
}

func (a *animeTorrents) listPageResponse(pageNumber int) (string, error) {
	log.Printf("Getting list page no.: %d\n", pageNumber)

	var buf io.Reader
	req, err := http.NewRequest("GET",
		fmt.Sprintf(torrentListURL, a.maxTorrentPages, pageNumber), buf)
	if err != nil {
		return "", fmt.Errorf(
			"failed creating new request for page no. %d %s",
			pageNumber, err.Error())
	}
	req.Header.Add("X-Requested-With", "XMLHttpRequest")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf(
			"failed to GET the page no. %d %s", pageNumber,
			err.Error())
	}
	defer resp.Body.Close()

	log.Printf("Response status code: %d\n", resp.StatusCode)
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf(
			"failed to read torrent page response body: %s",
			err.Error())
	}

	log.Printf("Return body length: %d", len(body))

	if strings.Contains(string(body), "Access Denied!") {
		return "", fmt.Errorf("failed to access torrent page %d",
			pageNumber)
	}

	return string(body), nil
}

func (a *animeTorrents) create() error {
	log.Println("Creating http client.")
	options := cookiejar.Options{PublicSuffixList: publicsuffix.List}
	jar, err := cookiejar.New(&options)
	if err != nil {
		return fmt.Errorf("failed to create cookiejar: %s",
			err.Error())
	}

	a.client = pester.New()
	a.client.Jar = jar
	a.client.Timeout = 30 * time.Second
	a.client.MaxRetries = 5

	return nil
}

func (a *animeTorrents) login() error {
	log.Println("Logging in.")
	if _, err := a.client.Get(loginURL); err != nil {
		return fmt.Errorf("failed to get login page: %s",
			err.Error())
	}

	params := url.Values{}
	params.Add("form", "login")
	params.Add("username", viper.GetString("loginUsername"))
	params.Add("password", viper.GetString("loginPassword"))
	resp, err := a.client.PostForm(loginURL, params)
	if err != nil {
		return fmt.Errorf("failed to post login data: %s",
			err.Error())
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read login response body: %s",
			err.Error())
	}

	// A known error text upon failed login.
	if strings.Contains(string(body),
		"Error: Invalid username or password.") {
		return errors.New("login failed: invalid username or password")
	}

	// If I can't see my username, then I am not logged in.
	if !strings.Contains(string(body), viper.GetString("loginUsername")) {
		return errors.New("login failed: can't find username in response body")
	}

	log.Println("Logged in.")

	return nil
}

func (a *animeTorrents) maxPages() error {
	log.Println("Finding out torrents max page number.")
	resp, err := a.client.Get(torrentsURL)
	if err != nil {
		return fmt.Errorf("failed to get the torrents page: %s",
			err.Error())
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read torrents list response body: %s",
			err.Error())
	}

	re := regexp.MustCompile(`ajax/torrents_data\.php\?total=(\d+)&page=1`)
	match := re.FindStringSubmatch(string(body))
	if len(match) > 1 && match[1] != "" {
		total, err := strconv.Atoi(match[1])
		if err != nil {
			return fmt.Errorf("can't convert %d to int", total)
		}
		log.Printf("Max pages figured out: %d.\n", total)
		a.maxTorrentPages = total
	}

	return nil
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
	b.WriteString(fmt.Sprintf(`<generator>Golang</generator>`))
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

func putOnS3(filePath string) error {
	sess, err := session.NewSession(&aws.Config{
		Endpoint: aws.String(viper.GetString("doSpacesEndpoint")),
		Region:   aws.String(viper.GetString("doSpacesRegion")),
		Credentials: credentials.NewStaticCredentials(
			viper.GetString("doSpacesKey"),
			viper.GetString("doSpacesSecret"),
			""),
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
		Bucket: aws.String(viper.GetString("doSpacesBucket")),
		Key:    aws.String(viper.GetString("doSpacesObjectName")),
		Body:   file,
		// https://docs.aws.amazon.com/AmazonS3/latest/dev/acl-overview.html#canned-acl
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
