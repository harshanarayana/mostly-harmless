package covfefe

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dghubble/go-twitter/twitter"
	"github.com/h2non/filetype"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type Message struct {
	account *twitter.User
	msg     []byte
	id      int64
}

func (c *Covfefe) processTweet(m *Message, tweet *twitter.Tweet) {
	new, err := c.insertTweet(tweet, m.id)
	if err != nil {
		log.WithFields(log.Fields{
			"err": err, "message": m.id, "tweet": tweet.ID,
		}).Error("Failed to insert tweet")
		return
	}
	if !new {
		return
	}

	c.processUser(m, tweet.User)

	var media []twitter.MediaEntity
	if tweet.Entities != nil {
		media = tweet.Entities.Media
	}
	if tweet.ExtendedEntities != nil {
		media = tweet.ExtendedEntities.Media
	}
	if tweet.ExtendedTweet != nil {
		if tweet.ExtendedTweet.Entities != nil {
			media = tweet.ExtendedTweet.Entities.Media
		}
		if tweet.ExtendedTweet.ExtendedEntities != nil {
			media = tweet.ExtendedTweet.ExtendedEntities.Media
		}
	}
	if len(media) != 0 && !c.rescan {
		c.wg.Add(1)
		go func() {
			for _, m := range media {
				if m.SourceStatusID != 0 {
					// We'll find this media attached to the retweet.
					continue
				}
				log := log.WithFields(log.Fields{
					"url": m.MediaURLHttps, "media": m.ID, "tweet": tweet.ID,
				})
				body, err := c.httpGet(m.MediaURLHttps)
				if err != nil {
					log.WithError(err).Error("Failed to download media")
					continue
				}
				if err := c.saveMedia(body, m.ID); err != nil {
					log.WithError(err).Error("Failed to save media")
					continue
				}
				// TODO: archive videos?
			}
			c.wg.Done()
		}()
	}

	if tweet.RetweetedStatus != nil {
		c.processTweet(m, tweet.RetweetedStatus)
	}
	if tweet.QuotedStatus != nil {
		c.processTweet(m, tweet.QuotedStatus)
	}
	// TODO: crawl thread, non-embedded linked tweets
}

func (c *Covfefe) saveMedia(data []byte, id int64) error {
	t, err := filetype.Match(data)
	if err != nil {
		return errors.WithStack(err)
	}
	name := filepath.Join(c.mediaPath, fmt.Sprintf("%d.%s", id, t.Extension))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return errors.WithStack(err)
	}
	if _, err := f.Write(data); err != nil {
		return errors.WithStack(err)
	}
	return errors.WithStack(f.Close())
}

func (c *Covfefe) processUser(m *Message, user *twitter.User) {
	if err := c.insertUser(user, m.id); err != nil {
		log.WithError(err).WithField("message", m.id).Error("Failed to insert user")
	}
	// Deprecated and removed, thankfully. It would break deduplication.
	// if user.Following {
	// 	if err := c.insertFollow(m.account.ID, user.ID, m.id); err != nil {
	// 		log.WithError(err).WithField("message", m.id).Error("Failed to insert follow")
	// 	}
	// }
}

func isProtected(message interface{}) bool {
	switch m := message.(type) {
	case *twitter.Tweet:
		if m.User.Protected {
			return true
		}
	case *twitter.Event:
		if (m.Source != nil && m.Source.Protected) ||
			(m.Target != nil && m.Target.Protected) ||
			(m.TargetObject != nil && m.TargetObject.User.Protected) {
			return true
		}
		switch m.Event {
		case "quoted_tweet":
		case "favorite", "unfavorite":
		case "favorited_retweet":
		case "retweeted_retweet":
		case "follow", "unfollow":
		case "user_update":
		case "list_created", "list_destroyed", "list_updated", "list_member_added",
			"list_member_removed", "list_user_subscribed", "list_user_unsubscribed":
			return true // lists can be private
		case "block", "unblock":
			return true
		case "mute", "unmute":
			return true
		default:
			log.WithField("event", m.Event).Warning("Unknown event type")
			return true // when in doubt...
		}
	}
	return false
}

func (c *Covfefe) HandleChan(messages <-chan *Message) {
	for m := range messages {
		c.Handle(m)
	}
}

func (c *Covfefe) Handle(m *Message) {
	msg := getMessage(m.msg)

	if isProtected(msg) {
		log.WithField("account", m.account.ScreenName).Debug("Dropped protected message")
		return
	}

	switch obj := msg.(type) {
	case *twitter.Tweet:
		if err := c.insertMessage(m); err != nil {
			log.WithError(err).WithField("tweet", obj.ID).Error("Failed to insert message")
			return
		}
		c.processTweet(m, obj)
	case *twitter.StatusDeletion:
		if err := c.insertMessage(m); err != nil {
			log.WithError(err).WithField("deletion", obj.ID).Error("Failed to insert message")
			return
		}
		log.WithField("id", obj.ID).Debug("Deleted Tweet")
		c.deletedTweet(obj.ID, m.id)
	case *twitter.Event:
		if err := c.insertMessage(m); err != nil {
			log.WithError(err).WithField("event", obj.Event).Error("Failed to insert message")
			return
		}
		if obj.Source != nil {
			c.processUser(m, obj.Source)
		}
		if obj.Target != nil {
			c.processUser(m, obj.Target)
		}
		if obj.TargetObject != nil {
			c.processTweet(m, obj.TargetObject)
		}
		if obj.Event == "follow" {
			if err := c.insertFollow(obj.Source.ID, obj.Target.ID, m.id); err != nil {
				log.WithError(err).WithField("message", m.id).Error("Failed to insert follow")
			}
		}

	case *twitter.StatusWithheld:
		if err := c.insertMessage(m); err != nil {
			log.WithError(err).Error("Failed to insert message")
		}
		log.WithFields(log.Fields{
			"id": strconv.FormatInt(obj.ID, 10), "user": strconv.FormatInt(obj.UserID, 10),
			"countries": strings.Join(obj.WithheldInCountries, ","),
		}).Info("Status withheld")
	case *twitter.UserWithheld:
		if err := c.insertMessage(m); err != nil {
			log.WithError(err).Error("Failed to insert message")
		}
		log.WithFields(log.Fields{
			"user":      strconv.FormatInt(obj.ID, 10),
			"countries": strings.Join(obj.WithheldInCountries, ","),
		}).Info("User withheld")

	default:
		log.Warningf("Unhandled message type: %T", msg)
	}
}

func (c *Covfefe) httpGet(url string) ([]byte, error) {
	// TODO: retry
	res, err := c.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}
