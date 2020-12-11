/*
Copyright © 2020 Sebastian Green-Husted <geoffcake@gmail.com>

*/
package cmd

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/apex/log"
	"github.com/bwmarrin/discordgo"
	"github.com/cenkalti/backoff/v4"
	libgiphy "github.com/sanzaru/go-giphy"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var inStock bool
var inQueue bool
var onlyAfter time.Time

// watchCmd represents the watch command
var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "A brief description of your command",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	Run: func(cmd *cobra.Command, args []string) {
		log.SetLevelFromString(viper.GetString("log_level"))
		c := make(chan os.Signal)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)

		err := backoff.Retry(func() error {
			discord, err := discordgo.New("Bot " + viper.GetString("bot_token"))
			if err != nil {
				log.Error(err.Error())
				return err
			}

			discord.AddHandler(messageCreate)

			err = discord.Open()
			if err != nil {
				log.Error(err.Error())
				return err
			}
			defer discord.Close()

			log.SetHandler(log.HandlerFunc(func(e *log.Entry) error {
				sendIt := time.Now().After(onlyAfter) || inStock
				if sendIt {
					_, anounceErr := discord.ChannelMessageSend(viper.GetString("channel"), fmt.Sprintf("%s: %s", e.Level, e.Message))
					if anounceErr != nil {
						// log.Error(anounceErr.Error())
						return anounceErr
					}
				}
				return nil
			}))

			log.Info("Initializing")
			gif, err := gifIt("start")
			if err == nil {
				discord.ChannelMessageSend(viper.GetString("channel"), gif)
			}

			if viper.GetBool("debug_mode") {
				gif, err := gifIt("testing")
				if err == nil {
					discord.ChannelMessageSend(viper.GetString("channel"), gif)
				}
			}

			onlyAfter = time.Now()
			timer := time.Tick(viper.GetDuration("interval") * time.Second)
			for {
				select {
				case <-c:
					log.Info("Shutting down")
					gif, err := gifIt("shut it down")
					if err == nil {
						discord.ChannelMessageSend(viper.GetString("channel"), gif)
					}

					os.Exit(0)
					return nil
				case <-timer:
					wasInQueue := inQueue
					wasInStock := inStock
					if time.Now().Before(onlyAfter) {
						continue
					}

					err := backoff.Retry(func() error { return checkAndNotify(discord) }, backoff.NewExponentialBackOff())
					if err != nil {
						log.Error(err.Error())
						return err
					}
					if (inStock || inQueue) && !(wasInStock || wasInQueue) {
						log.Info("In queue, slowing down")
						timer = time.Tick(viper.GetDuration("recheck_interval") * time.Second)
					} else if !(inStock || inQueue) && (wasInQueue || wasInStock) {
						log.Info("Gone again, speeding up")
						timer = time.Tick(viper.GetDuration("interval") * time.Second)
					}
				}
			}
		}, backoff.NewExponentialBackOff())
		if err != nil {
			log.Error(err.Error())
			return
		}
	},
}

func init() {
	rootCmd.AddCommand(watchCmd)
}

type SonyDirectProductResponse struct {
	CurrentPage int `json:"currentPage"`
	Products    []struct {
		BaseProduct       string `json:"baseProduct"`
		CategoryHierarchy []struct {
			Code string `json:"code"`
			Name string `json:"name"`
		} `json:"categoryHierarchy"`
		Code                 string `json:"code"`
		CompatibilityNotices []struct {
			IsBold bool   `json:"isBold"`
			Text   string `json:"text"`
		} `json:"compatibilityNotices"`
		LegalDisclosure       string `json:"legalDisclosure"`
		LoginGated            bool   `json:"loginGated"`
		LongDescription       string `json:"longDescription"`
		LongDescriptionHeader string `json:"longDescriptionHeader"`
		MaxOrderQuantity      int    `json:"maxOrderQuantity"`
		Name                  string `json:"name"`
		Overline              string `json:"overline"`
		PreOrderProduct       bool   `json:"preOrderProduct"`
		Price                 struct {
			BasePrice      string  `json:"basePrice"`
			CurrencyIso    string  `json:"currencyIso"`
			CurrencySymbol string  `json:"currencySymbol"`
			DecimalPrice   string  `json:"decimalPrice"`
			Value          float64 `json:"value"`
		} `json:"price"`
		PrimaryCategoryName string   `json:"primaryCategoryName"`
		Purchasable         bool     `json:"purchasable"`
		ReleaseDateDisplay  string   `json:"releaseDateDisplay"`
		SieProductFeatures  []string `json:"sieProductFeatures"`
		Stock               struct {
			StockLevelStatus string `json:"stockLevelStatus"`
		} `json:"stock"`
		StreetDate       time.Time `json:"streetDate"`
		URL              string    `json:"url"`
		ValidProductCode bool      `json:"validProductCode"`
	} `json:"products"`
	TotalPageCount    int `json:"totalPageCount"`
	TotalProductCount int `json:"totalProductCount"`
}

var checks int
var lastChecked time.Time

func checkAndNotify(s *discordgo.Session) error {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("OOPS! It exploded! %s", r)
		}
	}()

	log.Debugf("Iteration %s", checks)

	url := "https://direct.playstation.com/en-us/consoles/console/playstation5-console.3005816"
	redirected, err := wouldEnqueue(url)
	if err != nil {
		log.Debug(err.Error())
		onlyAfter = time.Now().Add(1 * time.Minute)
		return nil
	}
	log.Debugf("redirect val %+v", redirected)

	response, fetchErr := pullProductData(viper.GetString("product"))
	if fetchErr != nil {
		return fetchErr
	}

	if len(response.Products) < 1 {
		onlyAfter = time.Now().Add(1 * time.Minute)
		return nil
	}
	stockStatus := response.Products[0].Stock.StockLevelStatus
	log.Debugf("Stock level %s", stockStatus)

	var gotOutOfStock bool
	if viper.GetBool("debug_mode") {
		gotOutOfStock = (checks%30) < 10 || (checks%30) > 15
		redirected = (checks%30) > 5 && (checks%30) < 20
	} else {
		gotOutOfStock = stockStatus == viper.GetString("block_status")
	}

	log.Debugf("OOS: %t REDIR: %t", gotOutOfStock, redirected)
	if redirected {
		if !inQueue {
			gif, err := gifIt("lets do this")
			if err == nil {
				s.ChannelMessageSend(viper.GetString("channel"), gif)
			}

			log.Infof("%s Got status %s. It's go time!", viper.GetString("notify"), stockStatus)
			inQueue = true
			openTabErr := exec.Command("xdg-open", url).Start()
			if openTabErr != nil {
				log.Error("Couldn't open a tab! Panic!")
				return openTabErr
			}

			log.Info("Opened a tab!")
			log.Infof("%s Click me if you want to try! %s", viper.GetString("notify"), url)
		}
	} else {
		if inQueue {
			log.Info("No longer doing redirect")
			inQueue = false
		}
	}

	if gotOutOfStock {
		if inStock {
			log.Info("Poo!  Looks like it's gone")
			inStock = false
		}
	} else {
		if !inStock {
			log.Info("Seems to be available!")
			inStock = true
		}
	}

	checks++

	lastChecked = time.Now()

	return nil
}

func pullProductData(code string) (SonyDirectProductResponse, error) {
	var response SonyDirectProductResponse
	req, err := http.NewRequest(
		"GET",
		fmt.Sprintf("https://api.direct.playstation.com/commercewebservices/ps-direct-us/users/anonymous/products/productList?fields=BASIC&productCodes=%s", code),
		nil,
	)
	if err != nil {
		log.Error(err.Error())
		return response, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Errorf("Fetching: %s", err.Error())
		return response, err
	}
	defer resp.Body.Close()

	body, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		log.Errorf("Reading: %s", readErr.Error())
		return response, readErr
	}

	jsonErr := json.Unmarshal(body, &response)
	if jsonErr != nil {
		xmlErr := xml.Unmarshal(body, &response)
		if xmlErr != nil {
			log.Errorf("Parsing: %s", jsonErr.Error())
			log.Info("Error preamble...")
			log.Info(string(body[:128]) + "...")
			fmt.Println(string(body))
			return response, jsonErr
		}
	}

	return response, nil
}

func wouldEnqueue(url string) (redirect bool, err error) {
	req, err := http.NewRequest(
		"GET",
		url,
		nil,
	)
	if err != nil {
		log.Error(err.Error())
		return
	}
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Errorf("Fetching: %s", err.Error())
		return
	}
	return resp.StatusCode >= 300 && resp.StatusCode < 400, nil
}

/* TODO
The commands should get turned into structures that can be tied to different or all channels.
Each command should register what arguments it takes, as well as help text and an action.
That way different commands have different arguments, and essentially parse themselves.
A datastore can be used to make basic self documenting commands
Ideal:
!know foo is gif bar
!know baz is fact qux
!know x is say y

then !wtf foobar whould say what a command is, and any help it has.

!know mayo is interject gif mayo .75
	if it sees the word mayo, 75% of the time interject with a mayo gif

For major fun, add the js interpreter, and let messages be dynamically processed.
goja or otto are strong contenders
*/
func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == viper.GetString("bot_id") {
		return
	}

	if m.ChannelID == viper.GetString("channel") {
		switch m.Content {
		case "!help":
			s.ChannelMessageSend(viper.GetString("channel"), `How to bot:
!help : this message
!status : report last check, and status of inventory
!terminate : shuts down the bot, it's gone crazy
!stfu : stop checks for 30 minutes
`)
		case "!status":
			s.ChannelMessageSend(viper.GetString("channel"), fmt.Sprintf("Last: %s\nIn Stock: %t", lastChecked, inStock))
		case "!stfu":
			s.ChannelMessageSend(viper.GetString("channel"), ":face_with_symbols_over_mouth: NO U")
			onlyAfter = time.Now().Add(30 * time.Minute)
		case "!exterminate":
			s.ChannelMessageSendTTS(viper.GetString("channel"), "EXTERMINATE")
			gif, err := gifIt("exterminate")
			if err == nil {
				s.ChannelMessageSend(viper.GetString("channel"), gif)
			}
		case "!firejeffbezosintothesun":
			gif, err := gifIt("bezos")
			if err == nil {
				s.ChannelMessageSend(viper.GetString("channel"), gif)
			}
		case "!terminate":
			s.ChannelMessageSend(viper.GetString("channel"), "My mind is going... I can feel it.")
			gif, err := gifIt("hal9000")
			if err == nil {
				s.ChannelMessageSend(viper.GetString("channel"), gif)
			}

			s.Close()
			os.Exit(0)
		default:
			if rand.Intn(10) == 5 {
				s.ChannelMessageSendTTS(viper.GetString("channel"), m.Content)
			}
		}
	}
}

func gifIt(term string) (string, error) {
	giphy := libgiphy.NewGiphy(viper.GetString("giphy_key"))
	// return giphy.GetSearch(term, 1, 0, "", "", false)

	data, err := giphy.GetRandom(term)
	if err != nil {
		return "", err
	}

	return data.Data.Url, nil
}
