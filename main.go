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
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
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
	Id      string
	Title   string
	Updated string
	Link    string
	Entry   []*feedEntry
}

// feedEntry represents a single article in a feed.
type feedEntry struct {
	Id       string
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
	regexpTorrentRows        = regexp.MustCompile(`(?m)<tr class="data(Odd|Even)[\s\S]*?<a[\s\w\W]+?title="(?P<category>.+?)"[\s\S]+?<a href="(?P<url>.+?)"[\s\S]+?<strong>(?P<title>.+?)</a>`)
	regexpImgTag             = regexp.MustCompile(`<img.+?/>`)
	regexpExcludedCategories = regexp.MustCompile(`(Manga|Novel|Doujin)`)
	regexpPlot               = regexp.MustCompile(`<div id="torDescription">[\s\w\W]*?</div>`)
	regexpCoverImageUrl      = regexp.MustCompile(`src="(https://animetorrents\.me/imghost/covers/.+?)"`)
	regexpScreenShotsSmall   = regexp.MustCompile(`src="(https://animetorrents\.me/imghost/screenthumb/.+?)"`)
	regexpScreenShotsLarge   = regexp.MustCompile(`href="(https://animetorrents\.me/imghost/screens/.+?)"`)
	regexpEntryUpdated       = regexp.MustCompile(`<span class="blogDate">(.*?])</span>`)
	fileBaseName             string
)

func init() {
	raven.SetDSN("https://f9af5d4b88bb4df1a182849a4387c61e:efeeca97b5dc4ef0b485c81651d183ce@sentry.io/1078203")

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
		raven.CaptureErrorAndWait(err, nil)
		log.Fatalf("%s\n", err.Error())
	}
}

func main() {
	telegram.Create(viper.GetString("telegram.botToken"),
		viper.GetInt("telegram.targetId"))

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
		<img src="{{.CoverImageUrl}}" />
		<p>[{{.Category}}]</p>
		<p><a href="{{.AbsoluteLink}}" target="blank">{{.AbsoluteLink}}</a></p>
		{{.Plot}}
		{{range .Screenshots}}
		<a href="{{ .large }}">
        <img src="{{ .small }}" width="200" height="100" />"
    </a>
		{{end}}
	`)
	if err != nil {
		raven.CaptureErrorAndWait(err, nil)
		log.Printf("Failed to parse template: %s\n", err.Error())
		telegram.Send(err.Error())
		return
	}

	feed := &atomFeed{
		Id:      "vadviktor.xyz",
		Updated: time.Now().Format(time.RFC3339),
		Link: fmt.Sprintf("https://s3-%s.amazonaws.com/%s/%s",
			viper.GetString("digitalocean.spacesRegion"),
			viper.GetString("digitalocean.spacesBucket"),
			viper.GetString("digitalocean.spacesObjectName")),
		Author: feedPerson{
			Name:  "Viktor (Ikon) VAD",
			Email: "vad.viktor@gmail.com",
			URI:   "https://www.github.com/vadviktor",
		},
		Title: "Animetorrents.me feed",
	}

	s3Session, err := session.NewSession(&aws.Config{
		Endpoint: aws.String(viper.GetString("digitalocean.spacesEndpoint")),
		Region:   aws.String(viper.GetString("digitalocean.spacesRegion")),
		Credentials: credentials.NewStaticCredentials(
			viper.GetString("digitalocean.spacesKey"),
			viper.GetString("digitalocean.spacesSecret"),
			""),
	})
	if err != nil {
		log.Printf("%s\n", err.Error())
		raven.CaptureErrorAndWait(err, nil)
		telegram.Send(err.Error())
		return
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
				Id:       results["url"],
				Category: results["category"],
			}

			// Get the profile for each selected item.
			time.Sleep(random(1, viper.GetInt("antiHammerMaxSleep")) * time.Second)
			log.Printf("Reading torrent profile: %s\n", results["url"])
			err := a.parseProfile(feedItem, results, entryContentTemplate, s3Session)
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

	err = putFeedOnS3(s3Session, tempFeedFile)
	if err != nil {
		raven.CaptureErrorAndWait(err, nil)
		telegram.Send(fmt.Sprintf("Failure during uploading file to S3: %s",
			err.Error()))
		log.Printf("Failure during uploading file to S3: %s\n", err.Error())

		return
	}

	telegram.SendSilent("Atom feed is ready.")
	log.Println("Script finished.")
}

// parseProfile extracts the content from the torrent profile and fills in the
// entry fields.
func (a *animeTorrents) parseProfile(feedItem *feedEntry,
		torrentRowInfo map[string]string, tpl *template.Template,
		s3 *session.Session) error {
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

	coverImageUrlMatch := regexpCoverImageUrl.FindStringSubmatch(bodyText)
	coverImageUrl, err := putImageOnS3(s3, coverImageUrlMatch[1])
	if err != nil {
		return fmt.Errorf("failed to upload image: %s", err.Error())
	}

	// Screenshot processing.
	var screenShots = map[string]map[string]string{}
	screenshotsLargeMatch := regexpScreenShotsLarge.FindAllSubmatch(body, -1)
	for _, m := range screenshotsLargeMatch {
		u := string(m[1])
		screenShots[path.Base(u)] = make(map[string]string)
		screenShots[path.Base(u)]["large"], err = putImageOnS3(s3, u)
		if err != nil {
			return fmt.Errorf("failed to upload image: %s", err.Error())
		}
	}
	screenshotsSmallMatch := regexpScreenShotsSmall.FindAllSubmatch(body, -1)
	for _, m := range screenshotsSmallMatch {
		u := string(m[1])
		screenShots[path.Base(u)] = make(map[string]string)
		screenShots[path.Base(u)]["small"], err = putImageOnS3(s3, u)
		if err != nil {
			return fmt.Errorf("failed to upload image: %s", err.Error())
		}
	}

	data := struct {
		CoverImageUrl string
		Category      string
		AbsoluteLink  string
		Plot          string
		Screenshots   map[string]map[string]string
	}{
		CoverImageUrl: coverImageUrl,
		Category:      torrentRowInfo["category"],
		AbsoluteLink:  torrentRowInfo["url"],
		Plot:          plotMatch,
		Screenshots:   screenShots,
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
			a.telegram.Send(fmt.Sprintf("Unable to parse time format: %s",
				err.Error()))
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
	b.WriteString("<?xml version='1.0' encoding='UTF-8'?>\n")
	b.WriteString("<feed xmlns=\"http://www.w3.org/2005/Atom\">\n")
	b.WriteString(fmt.Sprintf("<title>%s</title>\n", f.Title))
	b.WriteString(fmt.Sprintf("<updated>%s</updated>\n", f.Updated))
	b.WriteString(fmt.Sprintf("<id>%s</id>\n", f.Link))
	b.WriteString(fmt.Sprintf("<link href=\"%s\" rel=\"self\" />\n", f.Link))
	b.WriteString(fmt.Sprintf("<generator>Golang</generator>\n"))
	b.WriteString("<author>\n")
	b.WriteString(fmt.Sprintf("<name>%s</name>\n", f.Author.Name))
	b.WriteString(fmt.Sprintf("<uri>%s</uri>\n", f.Author.URI))
	b.WriteString(fmt.Sprintf("<email>%s</email>\n", f.Author.Email))
	b.WriteString("</author>\n")

	for _, e := range f.Entry {
		b.WriteString(fmt.Sprintf("<entry>\n"))
		b.WriteString(fmt.Sprintf("<title>%s</title>\n", e.Title))
		b.WriteString(fmt.Sprintf("<link href=\"%s\" rel=\"self\" />\n", e.Link))
		b.WriteString(fmt.Sprintf("<id>%s</id>\n", e.Link))
		b.WriteString(fmt.Sprintf("<updated>%s</updated>\n", e.Updated))
		b.WriteString(fmt.Sprintf("<content type=\"html\">%s</content>\n", e.Content))
		b.WriteString(fmt.Sprintf("</entry>\n"))
	}

	b.WriteString(`</feed>`)

	return b.Bytes()
}

func putFeedOnS3(s3Session *session.Session, filePath string) error {
	log.Printf("Uploading %s to S3.\n", filePath)

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	uploader := s3manager.NewUploader(s3Session)
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(viper.GetString("digitalocean.spacesBucket")),
		Key:    aws.String(viper.GetString("digitalocean.spacesObjectName")),
		Body:   file,
		// https://docs.aws.amazon.com/AmazonS3/latest/dev/acl-overview.html#canned-acl
		ACL:             aws.String("public-read"),
		ContentType:     aws.String("application/atom+xml"),
		ContentEncoding: aws.String("utf-8"),
	})

	return err
}

// putImageOnS3 downloads the image from the url, stores it in S3,
// then returns the S3 url for that image.
// The image will be checked first and only uploaded if it does not already
// exist.
// There is a builtin backoff retry added for SlowDown errors.
func putImageOnS3(s3Session *session.Session, url string) (string, error) {
	log.Printf("Uploading %s to S3.\n", url)

	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("IMG - couldn't download image from url: %s/n%s",
			url, err.Error())
	}
	defer resp.Body.Close()

	p := strings.Split(url, "/")
	// image key e.g.: prefix/2017/02/coverimg.jpg
	key := fmt.Sprintf("%s/%s/%s/%s",
		viper.GetString("digitalocean.spacesImagePrefix"),
		p[len(p)-3], p[len(p)-2], p[len(p)-1])

	svc := s3.New(s3Session)
	result, err := svc.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(viper.GetString("digitalocean.spacesBucket")),
		Key:    aws.String(key),
	})
	if err == nil {
		log.Printf("image already exists with size of %d bytes\n",
			result.ContentLength)

		newUrl := fmt.Sprintf("%s/%s",
		viper.GetString("digitalocean.spacesBaseUrl"), key)
		return newUrl, err
	}

	backoffSleep := 1
	maxRetry := 5
	uploader := s3manager.NewUploader(s3Session)
	for retryCounter := 1; retryCounter <= maxRetry; retryCounter++ {
		time.Sleep(time.Second * time.Duration(backoffSleep))
		_, err = uploader.Upload(&s3manager.UploadInput{
			Bucket: aws.String(viper.GetString("digitalocean.spacesBucket")),
			Key:    aws.String(key),
			Body:   resp.Body,
			// https://docs.aws.amazon.com/AmazonS3/latest/dev/acl-overview.html#canned-acl
			ACL: aws.String("public-read"),
		})

		// https://github.com/aws/aws-sdk-go/blob/master/example/aws/request/handleServiceErrorCodes/handleServiceErrorCodes.go#L52:30
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "SlowDown":
				if retryCounter == maxRetry {
					return "", fmt.Errorf("s3 - %s", err.Error())
				}

				// Got a slow down message, need to retry.
				backoffSleep += 1
				continue
			default:
				return "", fmt.Errorf("s3 - %s", err.Error())
			}
		}

		// No error, we can quit the loop.
		break
	}

	newUrl := fmt.Sprintf("%s/%s",
		viper.GetString("digitalocean.spacesBaseUrl"), key)
	return newUrl, err
}

func random(min, max int) time.Duration {
	rand.Seed(time.Now().UnixNano())
	return time.Duration(rand.Intn(max-min) + min)
}
