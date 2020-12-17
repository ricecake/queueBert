/*
Copyright © 2020 Sebastian Green-Husted <geoffcake@gmail.com>

*/
package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/apex/log"
	"github.com/bwmarrin/discordgo"
	"github.com/cenkalti/backoff/v4"
	"github.com/davecgh/go-spew/spew"
	wr "github.com/mroth/weightedrand"
	libgiphy "github.com/sanzaru/go-giphy"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var onlyAfter time.Time
var moods *wr.Chooser

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

		choices := []wr.Choice{}
		for mood, rawWeight := range viper.GetStringMap("moods") {
			choices = append(choices, wr.Choice{
				Item:   mood,
				Weight: uint(rawWeight.(int)),
			})
		}
		chooser, chooserErr := wr.NewChooser(choices...)
		if chooserErr != nil {
			fmt.Println(chooserErr)
			return
		}
		moods = chooser

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
				sendIt := time.Now().After(onlyAfter)
				if sendIt {
					_, anounceErr := discord.ChannelMessageSend(viper.GetString("channel"), fmt.Sprintf("%s: %s", e.Level, e.Message))
					if anounceErr != nil {
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

/* TODO
The commands should get turned into structures that can be tied to different or all channels.
Each command should register what arguments it takes, as well as help text and an action.
That way different commands have different arguments, and essentially parse themselves.
A datastore can be used to make basic self documenting commands
Ideal:
!know foo is gif bar
!know baz is fact qux
!know x is say y

!wdyk <fact key>? will dump facts, or specific values

then !wtf foobar whould say what a command is, and any help it has.

!know mayo is interject gif mayo .75
	if it sees the word mayo, 75% of the time interject with a mayo gif

For major fun, add the js interpreter, and let messages be dynamically processed.
goja or otto are strong contenders

need it to build a list of named commands, and have the ability to have a list of "every message" commands
those will get run on any message that doesn't match a specific command.


ADD SOME TIMERSTUFF

Make it so that when it starts, it sets a random timer that published a message on a channel that sends through 'actions', a class of some sort.
If it gets a message on the channel, it should do what that action specifies.
Can start multiple timers in goroutines that can each publish an action on a timer, and have an action also do the needful and start a new timer.
That way different things can be recurring.
Want a way to post random catfacts at different intervals, as well as fortunes, or images from odd twitter accounts and subreddits.
*/
func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == viper.GetString("bot_id") {
		return
	}

	if viper.GetBool("debug") {
		spew.Dump(m)
	}

	switch m.Content {
	case "!help":
		s.ChannelMessageSend(m.ChannelID, `How to bot:
!help : this message
!status : report last check, and status of inventory
!terminate : shuts down the bot, it's gone crazy
!stfu : stop checks for 30 minutes
`)
	case "!status":
		mood := moods.Pick().(string)
		gif, err := gifIt(mood)
		if err == nil {
			s.ChannelMessageSend(m.ChannelID, gif)
		}

	case "!stfu":
		s.ChannelMessageSend(m.ChannelID, ":face_with_symbols_over_mouth: NO U")
		onlyAfter = time.Now().Add(30 * time.Minute)
	case "!exterminate":
		s.ChannelMessageSendTTS(m.ChannelID, "EXTERMINATE")
		gif, err := gifIt("exterminate")
		if err == nil {
			s.ChannelMessageSend(m.ChannelID, gif)
		}
	case "!firejeffbezosintothesun":
		gif, err := gifIt("bezos")
		if err == nil {
			s.ChannelMessageSend(m.ChannelID, gif)
		}
	case "!terminate":
		s.ChannelMessageSend(m.ChannelID, "My mind is going... I can feel it.")
		gif, err := gifIt("hal9000")
		if err == nil {
			s.ChannelMessageSend(m.ChannelID, gif)
		}

		s.Close()
		os.Exit(0)
	default:
		// TODO: Make thie convert to sarcasm case, and be less frequent than 1/20
		// if rand.Intn(500) == 5 {
		// 	s.ChannelMessageSendTTS(m.ChannelID, m.Content)
		// }
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

func MatchToMap(re *regexp.Regexp, input string) map[string]string {
	values := re.FindStringSubmatch(input)
	keys := re.SubexpNames()

	output := make(map[string]string)
	for i := 1; i < len(keys); i++ {
		if i < len(values) {
			output[keys[i]] = values[i]
		}
	}

	return output
}
