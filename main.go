// retrieve my paid content from https://www.brunobarbieri.blog/barbieriplus-membri/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/akiomik/vimeo-dl/vimeo"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/kirsle/configdir"
	"github.com/spf13/pflag"
	"mvdan.cc/xurls/v2"
)

// TODO add screenshotAsPDF to config file

const (
	progname = "bbplus"
	loginURL = "https://www.brunobarbieri.blog/login/"
	itemsURL = "https://www.brunobarbieri.blog/barbieriplus-membri/"
)

var defaultConfigFile = path.Join(configdir.LocalConfig(progname), "config.json")

var (
	flagDebug               = pflag.BoolP("debug", "d", false, "Enable debug log")
	flagConfigFile          = pflag.StringP("config", "c", defaultConfigFile, "Configuration file")
	flagShowBrowser         = pflag.BoolP("show-browser", "b", false, "show browser, useful for debugging")
	flagChromePath          = pflag.StringP("chrome-path", "C", "", "Custom path for chrome browser")
	flagExpectCookiesPrompt = pflag.BoolP("expect-cookies-prompt", "e", true, "If true, will wait for the cookies notice and decline it")
	flagProxy               = pflag.StringP("proxy", "P", "", "HTTP proxy")
	flagTimeout             = pflag.DurationP("timeout", "t", 2*time.Hour, "Global timeout as a parsable string (e.g. 1h12m)")
	flagOutdir              = pflag.StringP("outdir", "O", "", "Output directory")
	flagJustPrintURLs       = pflag.BoolP("just-print-urls", "J", false, "Just print URLs without downloading")
	flagScreenshotAsPDF     = pflag.BoolP("as-pdf", "p", false, "Save screenshot as PDF instead of PNG")
	flagDisableGPU          = pflag.BoolP("disable-gpu", "g", false, "Pass --disable-gpu to chrome")
)

// Config contains this program's configuration.
type Config struct {
	Proxy               string `json:"proxy"`
	Username            string `json:"username"`
	Password            string `json:"password"`
	ExpectCookiesPrompt bool   `json:"expect_cookies_prompt"`
	Outdir              string `json:"outdir"`
}

func loadConfig(configFile string) (*Config, error) {
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

	configPath := filepath.Dir(configFile)
	if configPath == "" {
		return nil, fmt.Errorf("configuration directory cannot be empty")
	}
	log.Printf("Trying to load config file %s", configFile)
	err := configdir.MakePath(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Configuration file does not exist, using defaults")
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
	config, err := loadConfig(*flagConfigFile)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}
	maskedPassword := "***"
	if config.Password == "" {
		maskedPassword = "<not set>"
	}
	log.Printf("Timeout                 : %s", *flagTimeout)
	log.Printf("Proxy                   : %s", config.Proxy)
	log.Printf("Show browser            : %v", *flagShowBrowser)
	log.Printf("Debug                   : %v", *flagDebug)
	log.Printf("Custom Chrome path      : %s", *flagChromePath)
	log.Printf("Username                : %s", config.Username)
	log.Printf("Password                : %s", maskedPassword)
	log.Printf("Expect cookies prompt   : %v", config.ExpectCookiesPrompt)
	log.Printf("Output directory        : %s", config.Outdir)
	log.Printf("Just print URLs         : %v", *flagJustPrintURLs)
	log.Printf("Disable GPU             : %v", *flagDisableGPU)
	ssformat := "PNG"
	if *flagScreenshotAsPDF {
		ssformat = "PDF"
	}
	log.Printf("Screenshot file format  : %s", ssformat)

	ctx, cancelFuncs := WithCancel(context.Background(), *flagTimeout, *flagShowBrowser, *flagDebug, *flagChromePath, config.Proxy, *flagDisableGPU)
	for _, cancel := range cancelFuncs {
		defer cancel()
	}
	if err := Login(ctx, config.Username, config.Password, config.ExpectCookiesPrompt); err != nil {
		log.Printf("Login failed: %v", err)
		return
	}
	if err := DownloadAll(ctx, config.Outdir, *flagJustPrintURLs, *flagScreenshotAsPDF); err != nil {
		log.Printf("DownloadAll failed: %v", err)
		return
	}
}

// Login logs into BB+.
func Login(ctx context.Context, username, password string, expectCookiesNotice bool) error {
	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}
	if password == "" {
		return fmt.Errorf("password cannot be empty")
	}
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
func DownloadAll(ctx context.Context, outDir string, justPrintURLs bool, screenshotAsPDF bool) error {
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
	)
	if screenshotAsPDF {
		tasks = append(tasks,
			chromedp.ActionFunc(func(ctx context.Context) error {
				buf, _, err := page.PrintToPDF().WithPrintBackground(false).Do(ctx)
				if err != nil {
					return err
				}
				data = buf
				return nil
			}),
		)
	} else {
		tasks = append(tasks, chromedp.FullScreenshot(&data, 90 /* PNG quality */))
	}

	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get item URLs or screenshots: %w", err)
	}
	screenshotFileName := filepath.Join(outDir, "index")
	if screenshotAsPDF {
		screenshotFileName += ".pdf"
	} else {
		screenshotFileName += ".png"
	}
	if err := os.WriteFile(screenshotFileName, data, 0644); err != nil {
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
			if err := Download(ctx, u, outDir, justPrintURLs, *flagScreenshotAsPDF); err != nil {
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
func Download(ctx context.Context, urlString, outDir string, justPrintURLs bool, screenshotAsPDF bool) error {
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
	screenshotFileName := filepath.Join(outDir, filePrefix)
	if screenshotAsPDF {
		screenshotFileName += ".pdf"
	} else {
		screenshotFileName += ".png"
	}
	log.Printf("Retrieving  %s", filePrefix)
	var data []byte
	tasks := chromedp.Tasks{
		chromedp.Navigate(urlString),
		chromedp.WaitVisible(`//*[contains(@class, 'elementor-heading-title')]`, chromedp.BySearch),
	}
	if screenshotAsPDF {
		tasks = append(tasks,
			chromedp.ActionFunc(func(ctx context.Context) error {
				buf, _, err := page.PrintToPDF().WithPrintBackground(false).Do(ctx)
				if err != nil {
					return err
				}
				data = buf
				return nil
			}),
		)
	} else {
		tasks = append(tasks, chromedp.FullScreenshot(&data, 90 /* PNG quality */))
	}

	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("screenshot failed: %w", err)
	}
	if err := os.WriteFile(screenshotFileName, data, 0644); err != nil {
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

func fetch(fileURL, filename string, cookies []*network.Cookie, referrer string) error {
	req, err := http.NewRequest("GET", fileURL, nil)
	if err != nil {
		return fmt.Errorf("http.NewRequest failed: %w", err)
	}
	for _, c := range cookies {
		var samesite http.SameSite
		switch c.SameSite {
		case network.CookieSameSiteStrict:
			samesite = http.SameSiteStrictMode
		case network.CookieSameSiteLax:
			samesite = http.SameSiteLaxMode
		case network.CookieSameSiteNone:
			samesite = http.SameSiteNoneMode
		default:
			samesite = http.SameSiteDefaultMode
		}
		req.AddCookie(&http.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Path:     c.Path,
			Domain:   c.Domain,
			Expires:  time.Unix(int64(c.Expires), 0),
			Secure:   c.Secure,
			HttpOnly: c.HTTPOnly,
			SameSite: samesite,
		})
	}
	if referrer != "" {
		req.Header.Set("Referer", referrer)
	}
	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("client.Do failed: %w", err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ReadAll failed: %w", err)
	}
	if err := os.WriteFile(filename, buf, 0644); err != nil {
		return fmt.Errorf("failed to save file '%s': %w", filename, err)
	}
	return nil
}

func fetchFromMasterJSON(masterJSONURL, filename string) error {
	mju, err := url.Parse(masterJSONURL)
	if err != nil {
		return fmt.Errorf("invalid master JSON URL: %w", err)
	}
	client := vimeo.NewClient()
	mj, err := client.GetMasterJson(mju)
	if err != nil {
		return fmt.Errorf("cannot get master JSON: %w", err)
	}
	// get video stream into a temp file
	videoFile, err := ioutil.TempFile("", progname)
	if err != nil {
		return fmt.Errorf("failed to create temp video file: %w", err)
	}
	defer func() {
		if err := videoFile.Close(); err != nil {
			log.Printf("Failed to close video file: %v", err)
		}
	}()
	log.Printf("Downloading %s video stream to %s", filename, videoFile.Name())
	if err := mj.CreateVideoFile(videoFile, mju, mj.FindMaximumBitrateVideo().Id, client); err != nil {
		return fmt.Errorf("failed to create video file for %s: %w", filename, err)
	}
	log.Printf("Downloaded %s video stream to %s", filename, videoFile.Name())
	// get audio stream into a temp file
	audioFile, err := ioutil.TempFile("", progname)
	if err != nil {
		return fmt.Errorf("failed to create temp audio file: %w", err)
	}
	defer func() {
		if err := audioFile.Close(); err != nil {
			log.Printf("Failed to close audio file: %v", err)
		}
	}()
	log.Printf("Downloading %s audio stream to %s", filename, audioFile.Name())
	if err := mj.CreateAudioFile(audioFile, mju, mj.FindMaximumBitrateAudio().Id, client); err != nil {
		return fmt.Errorf("failed to create audio file for %s: %w", filename, err)
	}
	log.Printf("Downloaded %s audio stream to %s", filename, videoFile.Name())
	// combine audio and video streams into one. Requires ffmpeg
	cmd := exec.Command("ffmpeg", "-y", "-i", videoFile.Name(), "-i", audioFile.Name(), "-c", "copy", filename)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to combine video and audio streams into %s: %w", filename, err)
	}
	return nil
}

// DownloadVideo downloads the video item.
func DownloadVideo(ctx context.Context, outDir, filePrefix string, justPrintURLs bool) error {
	log.Printf("Downloading video: %s.mp4", filePrefix)
	iframeSelector := `//iframe[contains(@class, 'elementor-video-iframe')]`
	var (
		iframeURL, iframeMainURL, iframeAlternateURL string
		found                                        bool
	)
	tasks := chromedp.Tasks{
		chromedp.WaitVisible(iframeSelector, chromedp.BySearch),
		chromedp.AttributeValue(iframeSelector, "src", &iframeMainURL, &found),
		chromedp.AttributeValue(iframeSelector, "suppressedsrc", &iframeAlternateURL, &found),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get iframe: %w", err)
	}
	if !found {
		return fmt.Errorf("iframe not found")
	}
	if !strings.HasPrefix(iframeMainURL, "https://") {
		// try the suppressedsrc field instead. In some cases it's the one that
		// contains the valid Vimeo URL
		if !strings.HasPrefix(iframeAlternateURL, "https://") {
			// the alternate URL is also broken?
			return fmt.Errorf("invalid video URLs: main: '%s', alternate: '%s'", iframeMainURL, iframeAlternateURL)
		}
		iframeURL = iframeAlternateURL
	} else {
		iframeURL = iframeMainURL
	}
	log.Printf("Navigating to iframe URL '%s'", iframeURL)
	playSelector := `//button[contains(@class, 'play')]`
	var (
		script  string
		cookies []*network.Cookie
		err     error
	)
	var currentURL string
	tasks = chromedp.Tasks{
		chromedp.Location(&currentURL),
		// set referrer on Vimeo private videos, or they won't load
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, _, err := page.Navigate(iframeURL).
				WithReferrer(currentURL).
				Do(ctx)
			return err
		}),
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
		chromedp.ActionFunc(func(ctx context.Context) error {
			cookies, err = network.GetAllCookies().Do(ctx)
			if err != nil {
				return err
			}
			return nil
		}),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get player script: %w", err)
	}

	rx := xurls.Strict()
	urls := rx.FindAllString(script, -1)
	// get the last URL ending with .mp4
	var mp4URL, masterJSONURL string
	for _, u := range urls {
		if strings.HasSuffix(u, ".mp4") {
			mp4URL = u
			break
		} else if strings.Contains(u, "/master.json?") {
			masterJSONURL = u
			break
		}
	}
	if mp4URL == "" && masterJSONURL == "" {
		return fmt.Errorf("empty video URL")
	}
	log.Printf("MP4 URL: %s\nMaster JSON URL: %s\n", mp4URL, masterJSONURL)
	if justPrintURLs {
		return nil
	}
	log.Printf("File prefix: %s", filePrefix)

	filename := filepath.Join(outDir, filePrefix+".mp4")
	log.Printf("Downloading video to '%s'", filename)
	if mp4URL != "" {
		return fetch(mp4URL, filename, cookies, currentURL)
	}
	return fetchFromMasterJSON(masterJSONURL, filename)
}

// DownloadPDF downloads the PDF item.
func DownloadPDF(ctx context.Context, outDir, filePrefix string, justPrintURLs bool) error {
	log.Printf("Retrieving PDF URL")
	var (
		pdfNodes []*cdp.Node
		cookies  []*network.Cookie
		err      error
	)
	tasks := chromedp.Tasks{
		chromedp.Nodes(`//a[contains(@target, '_blank')]`, &pdfNodes, chromedp.AtLeast(1), chromedp.BySearch),
		chromedp.ActionFunc(func(ctx context.Context) error {
			cookies, err = network.GetAllCookies().Do(ctx)
			if err != nil {
				return err
			}
			return nil
		}),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return fmt.Errorf("failed to get PDF node: %w", err)
	}
	pdfURL := pdfNodes[0].AttributeValue("href")
	if justPrintURLs {
		fmt.Println(pdfURL)
		return nil
	}
	filename := filepath.Join(outDir, filePrefix+".pdf")
	log.Printf("Downloading PDF '%s' to '%s'", pdfURL, filename)
	return fetch(pdfURL, filename, cookies, "")
}

// WithCancel returns a chromedp context with a cancellation function.
func WithCancel(ctx context.Context, timeout time.Duration, showBrowser, doDebug bool, chromePath, proxyURL string, disableGPU bool) (context.Context, []func()) {
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
	if disableGPU {
		allocatorOpts = append(allocatorOpts, chromedp.Flag("disable-gpu", disableGPU))
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
