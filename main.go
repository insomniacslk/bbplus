// retrieve my paid content from https://www.brunobarbieri.blog/barbieriplus-membri/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"

	"github.com/kirsle/configdir"
	"github.com/spf13/pflag"
	"mvdan.cc/xurls/v2"
)

const progname = "bbplus"

const (
	loginURL = "https://www.brunobarbieri.blog/login/"
	itemsURL = "https://www.brunobarbieri.blog/barbieriplus-membri/"
)

var (
	flagDebug               = pflag.BoolP("debug", "d", false, "Enable debug log")
	flagShowBrowser         = pflag.BoolP("show-browser", "b", false, "show browser, useful for debugging")
	flagChromePath          = pflag.StringP("chrome-path", "c", "", "Custom path for chrome browser")
	flagExpectCookiesPrompt = pflag.BoolP("expect-cookies-prompt", "C", true, "If true, will wait for the cookies notice and decline it")
	flagProxy               = pflag.StringP("proxy", "P", "", "HTTP proxy")
	flagTimeout             = pflag.UintP("timeout", "t", 120, "Timeout in seconds")
	flagOutdir              = pflag.StringP("outdir", "O", "", "Output directory")
	flagJustPrintURLs       = pflag.BoolP("just-print-urls", "J", false, "Just print URLs without downloading")
)

// Config contains this program's configuration.
type Config struct {
	Proxy               string `json:"proxy"`
	Username            string `json:"username"`
	Password            string `json:"password"`
	ExpectCookiesPrompt bool   `json:"expect_cookies_prompt"`
	Outdir              string `json:"outdir"`
}

func loadConfig() (*Config, error) {
	// default config. If the config file exists, it might override some fields.
	cfg := Config{}

	// ensure that command-line flags override config-file flags
	defer func() {
		if *flagProxy != cfg.Proxy {
			cfg.Proxy = *flagProxy
		}
		if *flagExpectCookiesPrompt != cfg.ExpectCookiesPrompt {
			cfg.ExpectCookiesPrompt = *flagExpectCookiesPrompt
		}
		if *flagOutdir != "" && *flagOutdir != cfg.Outdir {
			cfg.Outdir = *flagOutdir
		}
		log.Printf("Config loaded")
	}()

	configPath := configdir.LocalConfig(progname)
	configFile := path.Join(configPath, "config.json")
	log.Printf("Trying to load config file %s", configFile)
	err := configdir.MakePath(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &cfg, nil
		}
		return nil, fmt.Errorf("failed to create config path '%s': %w", configPath, err)
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &cfg, nil
		}
		return nil, fmt.Errorf("failed to open '%s': %w", configFile, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config file: %w", err)
	}

	return &cfg, nil
}

func main() {
	usage := func() {
		fmt.Fprintf(os.Stderr, "%s: fetch your paid Barbieri+ content.\n\n", progname)
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
		pflag.PrintDefaults()
		os.Exit(1)
	}
	pflag.Usage = usage
	pflag.Parse()
	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	timeout := time.Duration(*flagTimeout) * time.Second

	log.Printf("Timeout                 : %s", timeout)
	log.Printf("Proxy                   : %s", config.Proxy)
	log.Printf("Show browser            : %v", *flagShowBrowser)
	log.Printf("Debug                   : %v", *flagDebug)
	log.Printf("Custom Chrome path      : %s", *flagChromePath)
	log.Printf("Username                : %s", config.Username)
	log.Printf("Expect cookies prompt   : %v", config.ExpectCookiesPrompt)
	log.Printf("Output directory        : %s", config.Outdir)
	log.Printf("Just print URLs         : %v", *flagJustPrintURLs)

	ctx, cancelFuncs := WithCancel(context.Background(), timeout, *flagShowBrowser, *flagDebug, *flagChromePath, config.Proxy)
	for _, cancel := range cancelFuncs {
		defer cancel()
	}
	if err := Login(ctx, config.Username, config.Password, config.ExpectCookiesPrompt); err != nil {
		log.Fatalf("Login failed: %v", err)
	}
	if err := DownloadAll(ctx, config.Outdir, *flagJustPrintURLs); err != nil {
		log.Fatalf("DownloadAll failed: %v", err)
	}
}

// Login logs into BB+.
func Login(ctx context.Context, username, password string, expectCookiesNotice bool) error {
	tasks := chromedp.Tasks{
		chromedp.Navigate(loginURL),
	}
	if expectCookiesNotice {
		tasks = append(tasks,
			chromedp.WaitVisible(`//button[contains(@class, 'iubenda-cs-reject-btn')]`, chromedp.BySearch),
			chromedp.Click(`//button[contains(@class, 'iubenda-cs-reject-btn')]`),
		)
	}
	usernameField := `//input[contains(@data-key, 'username')]`
	passwordField := `//input[contains(@data-key, 'user_password')]`
	tasks = append(tasks,
		chromedp.WaitVisible(usernameField, chromedp.BySearch),
		chromedp.SendKeys(usernameField, username),
		chromedp.WaitVisible(passwordField, chromedp.BySearch),
		chromedp.SendKeys(passwordField, password),
		chromedp.Submit(passwordField),
		chromedp.WaitVisible(`//div[contains(@class, 'um-main-meta')]`, chromedp.BySearch),
	)
	return chromedp.Run(ctx, tasks)
}

// DownloadAll downloads all the user content of BB+.
func DownloadAll(ctx context.Context, outDir string, justPrintURLs bool) error {
	tasks := chromedp.Tasks{
		chromedp.Navigate(itemsURL),
	}
	linkFields := `//a[contains(@class, 'elementor-post__thumbnail__link')]`
	var (
		linkNodes []*cdp.Node
		data      []byte
	)
	tasks = append(tasks,
		chromedp.WaitVisible(linkFields, chromedp.BySearch),
		chromedp.Nodes(linkFields, &linkNodes, chromedp.AtLeast(0), chromedp.BySearch),
		chromedp.FullScreenshot(&data, 90 /* PNG quality */),
	)
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get item URLs: %w", err)
	}
	if err := ioutil.WriteFile("index.png", data, 0644); err != nil {
		return fmt.Errorf("failed to screenshot index: %w", err)
	}
	log.Printf("Screenshot saved to index.png")
	// create output directory
	if err := os.MkdirAll(outDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create output directory '%s': %w", outDir, err)
	}
	for _, n := range linkNodes {
		u, exists := n.Attribute("href")
		if exists {
			if err := Download(ctx, u, outDir, justPrintURLs); err != nil {
				return fmt.Errorf("failed to download '%s': %w", u, err)
			}
		}
	}
	return nil
}

// MediaType represents a Barbieri+ media type.
type MediaType int

// Actual media types
const (
	Unknown MediaType = iota
	Video
	PDF
)

// Download downloads an individual item.
func Download(ctx context.Context, urlString, outDir string, justPrintURLs bool) error {
	u, err := url.Parse(urlString)
	if err != nil {
		return fmt.Errorf("invalid URL '%s': %w", urlString, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 {
		return fmt.Errorf("empty path component, cannot derive file name")
	}
	filePrefix := parts[len(parts)-1]
	if filePrefix == "" {
		return fmt.Errorf("empty file prefix")
	}
	// save a full-page screenshot
	screenshotFileName := filepath.Join(outDir, filePrefix+".png")
	log.Printf("Retrieving  %s", filePrefix)
	var data []byte
	tasks := chromedp.Tasks{
		chromedp.Navigate(urlString),
		chromedp.WaitVisible(`//*[contains(@class, 'elementor-heading-title')]`, chromedp.BySearch),
		chromedp.FullScreenshot(&data, 90 /* PNG quality */),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("screenshot failed: %w", err)
	}
	if err := ioutil.WriteFile(screenshotFileName, data, 0644); err != nil {
		return fmt.Errorf("failed to write file '%s': %w", screenshotFileName, err)
	}
	log.Printf("Screenshot saved at '%s'", screenshotFileName)

	// now save the media (PDF or MP4)
	var containers []*cdp.Node
	containerSelector := `//*[contains(@class, 'elementor-location-single')]`
	tasks = chromedp.Tasks{
		chromedp.WaitVisible(containerSelector, chromedp.BySearch),
		chromedp.Nodes(containerSelector, &containers, chromedp.AtLeast(1), chromedp.BySearch),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get media container: %w", err)
	}
	// detect media type
	attr := containers[0].AttributeValue("class")
	classes := strings.Split(attr, " ")
	mediaType := Unknown
	for _, class := range classes {
		switch class {
		case "category-membership-pillola-video", "category-membership-videoricetta":
			mediaType = Video
		case "category-membership-dispensa-testo":
			mediaType = PDF
		default:
			continue
		}
	}
	switch mediaType {
	case Video:
		err = DownloadVideo(ctx, outDir, filePrefix, justPrintURLs)
	case PDF:
		err = DownloadPDF(ctx, outDir, filePrefix, justPrintURLs)
	case Unknown:
		err = fmt.Errorf("unknown media type")
	}
	return err
}

func fetch(fileURL, filename string) error {
	resp, err := http.Get(fileURL)
	if err != nil {
		return fmt.Errorf("http GET failed: %w", err)
	}
	defer resp.Body.Close()
	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ReadAll failed: %w", err)
	}
	if err := ioutil.WriteFile(filename, buf, 0644); err != nil {
		return fmt.Errorf("failed to save file '%s': %w", filename, err)
	}
	return nil
}

// DownloadVideo downloads the video item.
func DownloadVideo(ctx context.Context, outDir, filePrefix string, justPrintURLs bool) error {
	log.Printf("Downloading video: %s.mp4", filePrefix)
	iframeSelector := `//iframe[contains(@class, 'elementor-video-iframe')]`
	var (
		iframeURL string
		found     bool
	)
	tasks := chromedp.Tasks{
		chromedp.WaitVisible(iframeSelector, chromedp.BySearch),
		chromedp.AttributeValue(iframeSelector, "src", &iframeURL, &found),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get iframe: %w", err)
	}
	if !found {
		return fmt.Errorf("iframe not found")
	}
	log.Printf("Navigating to iframe URL '%s'", iframeURL)
	playSelector := `//button[contains(@class, 'play')]`
	var script string
	tasks = chromedp.Tasks{
		chromedp.Navigate(iframeURL),
		chromedp.WaitVisible(playSelector, chromedp.BySearch),
		chromedp.Click(playSelector),
		chromedp.Sleep(time.Second),
		chromedp.ActionFunc(func(ctx context.Context) error {
			htmlNode, err := dom.GetDocument().Do(ctx)
			if err != nil {
				return fmt.Errorf("GetDocument failed: %w", err)
			}
			script, err = dom.GetOuterHTML().WithNodeID(htmlNode.NodeID).Do(ctx)
			return err
		}),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get player script: %w", err)
	}

	rx := xurls.Strict()
	urls := rx.FindAllString(script, -1)
	// get the last URL ending with .mp4
	var mp4URL string
	for _, u := range urls {
		if strings.HasSuffix(u, ".mp4") {
			mp4URL = u
		}
	}
	if justPrintURLs {
		fmt.Println(mp4URL)
		return nil
	} else {
		filename := filepath.Join(outDir, filePrefix+".mp4")
		log.Printf("Starting download of '%s' into '%s'", mp4URL, filename)
		return fetch(mp4URL, filename)
	}
}

// DownloadPDF downloads the PDF item.
func DownloadPDF(ctx context.Context, outDir, filePrefix string, justPrintURLs bool) error {
	log.Printf("Retrieving PDF URL")
	var pdfNodes []*cdp.Node
	tasks := chromedp.Tasks{
		chromedp.Nodes(`//a[contains(@target, '_blank')]`, &pdfNodes, chromedp.AtLeast(1), chromedp.BySearch),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get PDF node: %w", err)
	}
	pdfURL := pdfNodes[0].AttributeValue("href")
	if justPrintURLs {
		fmt.Println(pdfURL)
		return nil
	} else {
		filename := filepath.Join(outDir, filePrefix+".pdf")
		log.Printf("Downloading PDF '%s' to '%s'", pdfURL, filename)
		return fetch(pdfURL, filename)
	}
}

// WithCancel returns a chromedp context with a cancellation function.
func WithCancel(ctx context.Context, timeout time.Duration, showBrowser, doDebug bool, chromePath, proxyURL string) (context.Context, []func()) {
	var cancelFuncs []func()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	cancelFuncs = append(cancelFuncs, cancel)

	// show browser
	var allocatorOpts []chromedp.ExecAllocatorOption
	if showBrowser {
		allocatorOpts = append(allocatorOpts, chromedp.NoFirstRun, chromedp.NoDefaultBrowserCheck)
	} else {
		allocatorOpts = append(allocatorOpts, chromedp.Headless)
	}
	if chromePath != "" {
		allocatorOpts = append(allocatorOpts, chromedp.ExecPath(chromePath))
	}
	if proxyURL != "" {
		allocatorOpts = append(allocatorOpts, chromedp.ProxyServer(proxyURL))
	}
	ctx, cancel = chromedp.NewExecAllocator(ctx, allocatorOpts...)
	cancelFuncs = append(cancelFuncs, cancel)

	var opts []chromedp.ContextOption
	if doDebug {
		opts = append(opts, chromedp.WithDebugf(log.Printf))
	}

	ctx, cancel = chromedp.NewContext(ctx, opts...)
	cancelFuncs = append(cancelFuncs, cancel)
	return ctx, cancelFuncs
}
