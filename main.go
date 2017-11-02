package main

import (
	"fmt"
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

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/publicsuffix"
)

const (
	username       = "vadviktor"
	password       = "rbT6uUuZDVYPb3SF"
	loginUrl       = "https://animetorrents.me/login.php"
	torrentsUrl    = "https://animetorrents.me/torrents.php"
	torrentListUrl = "https://animetorrents.me/ajax/torrents_data.php?total=%d&page=%d"
)

type animerTorrents struct {
	client          *http.Client
	maxTorrentPages int
}

func main() {
	a := &animerTorrents{}
	a.create()
	a.login()
	a.maxPages()

	log.Println("Start to parse torrent list pages.")
	for i := 1; i < 2; i++ {
		time.Sleep(5) // anti server-hammer
		resp := a.listPageResponse(i)
		doc, err := goquery.NewDocumentFromResponse(resp)
		if err != nil {
			log.Fatalf("Failed to create a GQuery document from page no.%s\n%s\n",
				i, err.Error())
		}

		// parse items, select which to get the profile for
		fmt.Println(doc.Find("table tr[class^=data]").Length())
		//doc.Find("table tr[class^=data]").Each(
		//	func(i int, s *goquery.Selection) {
		//		title := s.Find("td:nth-child(2) a:nth-child(1)")
		//		fmt.Println(title)
		//	})

		// get the profile for each selected item

		// save profile data

	}

	// compose final rss
}

func (a *animerTorrents) listPageResponse(pageNumber int) *http.Response {
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
		log.Fatalf("Failed to GET the page: %s\n%s\n", pageNumber, err.Error())
	}

	log.Printf("Response status code: %d\n", resp.StatusCode)
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read torrent page response body: %s\n", err.Error())
	}

	log.Printf("Return body length: %d", len(body))

	if strings.Contains(string(body), "Access Denied!") {
		log.Fatalf("Failed to access torrent page %d", pageNumber)
	}

	return resp
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

	//log.Printf("Page after login attempt:\n%s\n", string(body))

	if strings.Contains(string(body),
		"Error: Invalid username or password.") {
		log.Fatalln("Login failed: invalid username or password.")
	}

	if !strings.Contains(string(body),
		"vadviktor") {
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
