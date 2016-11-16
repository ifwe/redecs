package main

// Originally adapted from https://github.com/awslabs/service-discovery-ecs-dns
// This service is meant to run as a singleton instance in concert with redecs_agent
// running on AWS ECS instances. This service will check Redis regularly for active
// service updates and rewrite the A records for services to only contain recently
// active ip addresses.

import (
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	"gopkg.in/redis.v5"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const checkInterval = 1 * time.Minute // how often to check Redis
const defaultTTL = 300                // seconds
const fetchLast = 300                 // seconds

var DNSName = "servicediscovery.local"

func logErrorAndFail(err error) {
	if err != nil {
		// logrus calls os.exit(1)
		log.Fatal(err)
	}
}

func logErrorNoFatal(err error) {
	if err != nil {
		log.Error(err)
	}
}

type config struct {
	HostedZoneId string
	RedisHost    string
}

var configuration config

func getDNSHostedZoneId() (string, error) {
	r53 := route53.New(session.New())
	params := &route53.ListHostedZonesByNameInput{
		DNSName: aws.String(DNSName),
	}

	zones, err := r53.ListHostedZonesByName(params)

	if err == nil && len(zones.HostedZones) > 0 {
		return aws.StringValue(zones.HostedZones[0].Id), nil
	}

	return "", err
}

// Modify (or create) the A record for this serviceName, adding the private IP of the host.
func modifyDNSRecord(serviceName string, ips []string) error {
	var err error
	r53 := route53.New(session.New())

	aValues := make([]*route53.ResourceRecord, 0)
	// Put all the IPs in one A record
	for _, ip := range ips {
		aValues = append(aValues, &route53.ResourceRecord{Value: aws.String(ip)})
	}

	// This API call creates a new DNS record for this service
	params := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String(route53.ChangeActionUpsert),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: aws.String(serviceName + "." + DNSName),
						// It creates an A record with the name of the service
						Type:            aws.String(route53.RRTypeA),
						ResourceRecords: aValues,
						TTL:             aws.Int64(defaultTTL),
					},
				},
			},
			Comment: aws.String("Service Discovery Created Record"),
		},
		HostedZoneId: aws.String(configuration.HostedZoneId),
	}
	_, err = r53.ChangeResourceRecordSets(params)
	logErrorNoFatal(err)
	log.Debug(fmt.Sprintf("Record %s.%s updated with %d records.", serviceName, DNSName, len(ips)))
	return err
}

var redisClient *redis.Client

func processServicePings(servicePings []string) {
	var serviceMap = make(map[string][]string)

	for _, servicePing := range servicePings {
		pingChunks := strings.Split(servicePing, "_")
		serviceName := pingChunks[0]
		serviceIp := pingChunks[1]
		serviceMap[serviceName] = append(serviceMap[serviceName], serviceIp)
	}

	for serviceName, serviceIps := range serviceMap {
		modifyDNSRecord(serviceName, serviceIps)
	}
}

func fetchActiveServices() {
	var servicePings []string
	var sum int
	var err error
	log.Debug("Fetching active services ...")

	// Fetch last "fetchLast" seconds of service pings
	now := time.Now().Unix()
	epoch := strconv.FormatInt(now - fetchLast, 10) // seconds (string)

	// Try several times with exponential backoff.
	sum = 1
	for {
		servicePings, err = redisClient.ZRangeByScore("redecs:service_pings", redis.ZRangeBy{Min:epoch, Max:"+inf"}).Result()

		if err == nil {
			break
		}
		if sum > 8 {
			// Bail out if this is failing.
			logErrorAndFail(err)
		}
		time.Sleep(time.Duration(sum) * time.Second)
		sum += 2
	}

	processServicePings(servicePings)

	log.Debug("Done fetching active services.")
}

func main() {
	var err error
	var sum int
	var zoneId string

	if len(os.Args) != 3 {
		err = errors.New(fmt.Sprintf("Usage: %s [domain] [Redis host]\n", os.Args[0]))
		logErrorAndFail(err)
	}

	DNSName = os.Args[1]
	configuration.RedisHost = os.Args[2]

	sum = 1
	for {
		// We try to get the Hosted Zone Id using exponential backoff
		zoneId, err = getDNSHostedZoneId()
		if err == nil {
			break
		}
		if sum > 8 {
			logErrorAndFail(err)
		}
		time.Sleep(time.Duration(sum) * time.Second)
		sum += 2
	}
	configuration.HostedZoneId = zoneId

	sum = 1
	for {
		// We try to get the Redis connection using exponential backoff
		redisClient = redis.NewClient(&redis.Options{
			Addr:       configuration.RedisHost + ":6379",
			Password:   "", // none
			DB:         0,  // default DB
			MaxRetries: 3,
		})
		// Check the connection.
		err := redisClient.Ping().Err()

		if err == nil {
			break
		}
		if sum > 8 {
			logErrorAndFail(err)
		}
		time.Sleep(time.Duration(sum) * time.Second)
		sum += 2
	}

	http.HandleFunc("/healthcheck", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK")
	})

	log.Fatal(http.ListenAndServe(":8080", nil))

	// check regularly, specified by checkInterval
	ticker := time.NewTicker(checkInterval)

	for {
		// TODO: Add Redis Pub/Sub
		fetchActiveServices()
		<-ticker.C
	}
}
