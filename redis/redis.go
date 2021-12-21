package redis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

const (
	CacheChannelName string        = "blocky_sync"
	CacheStorePrefix string        = "blocky:cache:"
	chanCap          int           = 1000
	cacheReason      string        = "EXTERNAL_CACHE"
	defaultCacheTime time.Duration = time.Duration(1 * time.Second)
)

// sendBuffer message
type bufferMessage struct {
	Key     string
	Message *dns.Msg
}

// redis pubsub message
type redisMessage struct {
	K string // key
	M []byte // message
	C []byte // client
}

// CacheChannel message
type CacheMessage struct {
	Key      string
	Response *model.Response
}

// Client for redis communication
type Client struct {
	config       *config.RedisConfig
	client       *redis.Client
	l            *logrus.Entry
	ctx          context.Context
	id           []byte
	sendBuffer   chan *bufferMessage
	CacheChannel chan *CacheMessage
}

// New creates a new redis client
func New(cfg *config.RedisConfig) (*Client, error) {
	// disable redis if no address is provided
	if cfg == nil || len(cfg.Address) == 0 {
		return nil, nil
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:            cfg.Address,
		Password:        cfg.Password,
		DB:              cfg.Database,
		MaxRetries:      cfg.ConnectionAttempts,
		MaxRetryBackoff: time.Duration(cfg.ConnectionCooldown),
	})
	ctx := context.Background()

	_, err := rdb.Ping(ctx).Result()
	if err == nil {
		var id []byte

		id, err = uuid.New().MarshalBinary()
		if err == nil {
			// construct client
			res := &Client{
				config:       cfg,
				client:       rdb,
				l:            log.PrefixedLog("redis"),
				ctx:          ctx,
				id:           id,
				sendBuffer:   make(chan *bufferMessage, chanCap),
				CacheChannel: make(chan *CacheMessage, chanCap),
			}

			// start channel handling go routine
			err = res.startup()

			return res, err
		}
	}

	return nil, err
}

// PublishCache publish cache to redis async
func (c *Client) PublishCache(key string, message *dns.Msg) {
	if len(key) > 0 && message != nil {
		c.sendBuffer <- &bufferMessage{
			Key:     key,
			Message: message,
		}
	}
}

// GetRedisCache reads the redis cache and publish it to the channel
func (c *Client) GetRedisCache() {
	c.l.Debug("GetRedisCache")
	go func() {
		iter := c.client.Scan(c.ctx, 0, prefixKey("*"), 0).Iterator()
		for iter.Next(c.ctx) {
			response, err := c.getResponse(iter.Val())
			if err == nil {
				if response != nil {
					c.CacheChannel <- response
				}
			} else {
				c.l.Error("GetRedisCache ", err)
			}
		}
	}()
}

// startup starts a new goroutine for subscription and translation
func (c *Client) startup() error {
	ps := c.client.Subscribe(c.ctx, CacheChannelName)

	_, err := ps.Receive(c.ctx)
	if err == nil {
		go func() {
			for {
				select {
				// recieved message from subscription
				case msg := <-ps.Channel():
					c.l.Debug("Received message: ", msg)
					// message is not empty
					if msg != nil && len(msg.Payload) > 0 {
						var rm redisMessage

						err = json.Unmarshal([]byte(msg.Payload), &rm)
						if err == nil {
							// message was sent from a different blocky instance
							if bytes.Compare(rm.C, c.id) != 0 {
								var cm *CacheMessage

								cm, err = convertMessage(&rm, 0)
								if err == nil {
									c.CacheChannel <- cm
								}
							}
						} else {
							c.l.Error("Conversion error: ", err)
						}
					}
					// publish message from buffer
				case s := <-c.sendBuffer:
					origRes := s.Message
					origRes.Compress = true

					binRes, pErr := origRes.Pack()
					if pErr == nil {
						binMsg, mErr := json.Marshal(redisMessage{
							K: s.Key,
							M: binRes,
							C: c.id,
						})

						if mErr == nil {
							c.client.Publish(c.ctx, CacheChannelName, binMsg)
						}

						c.client.Set(c.ctx,
							prefixKey(s.Key),
							binRes,
							c.getTTL(origRes))
					}
				}
			}
		}()
	}

	return err
}

// getResponse returns model.Response for a key
func (c *Client) getResponse(key string) (*CacheMessage, error) {
	resp, err := c.client.Get(c.ctx, key).Result()
	if err == nil {
		var ttl time.Duration
		ttl, err = c.client.TTL(c.ctx, key).Result()
		if err == nil {
			var result *CacheMessage

			result, err = convertMessage(&redisMessage{
				K: cleanKey(key),
				M: []byte(resp),
			}, ttl)
			if err == nil {
				return result, nil
			}

		}
	}

	c.l.Error("Conversion error: ", err)

	return nil, err
}

// convertMessage converts redisMessage to CacheMessage
func convertMessage(message *redisMessage, ttl time.Duration) (*CacheMessage, error) {
	dns := dns.Msg{}

	err := dns.Unpack(message.M)
	if err == nil {
		if ttl > 0 {
			for _, a := range dns.Answer {
				a.Header().Ttl = uint32(ttl.Seconds())
			}
		}
		res := &CacheMessage{
			Key: message.K,
			Response: &model.Response{
				RType:  model.ResponseTypeCACHED,
				Reason: cacheReason,
				Res:    &dns,
			},
		}

		return res, nil
	}

	return nil, err
}

// getTTL of dns message or return defaultCacheTime if 0
func (c *Client) getTTL(dns *dns.Msg) time.Duration {
	ttl := uint32(0)
	for _, a := range dns.Answer {
		if a.Header().Ttl > ttl {
			ttl = a.Header().Ttl
		}
	}
	if ttl == 0 {
		return defaultCacheTime
	}

	return time.Duration(ttl) * time.Second
}

// prefixKey with CacheStorePrefix
func prefixKey(key string) string {
	return fmt.Sprintf("%s%s", CacheStorePrefix, key)
}

// cleanKey trims CacheStorePrefix prefix
func cleanKey(key string) string {
	return strings.TrimPrefix(key, CacheStorePrefix)
}