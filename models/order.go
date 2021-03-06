package models

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"log"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Microsoft/ApplicationInsights-Go/appinsights"
	amqp091 "github.com/streadway/amqp"
	"gopkg.in/matryer/try.v1"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	amqp10 "pack.ag/amqp"
)

// Order represents the order json
type Order struct {
	OrderID           string  `required:"false" description:"CosmoDB ID - will be autogenerated"`
	EmailAddress      string  `required:"true" description:"Email address of the customer"`
	PreferredLanguage string  `required:"false" description:"Preferred Language of the customer"`
	Product           string  `required:"false" description:"Product ordered by the customer"`
	Partition         string  `required:"false" description:"MongoDB Partition. Generated."`
	Total             float64 `required:"false" description:"Order total"`
	Source            string  `required:"false" description:"Source backend e.g. App Service, Container instance, K8 cluster etc"`
	Status            string  `required:"true" description:"Order Status"`
}

// Environment variables
var customInsightsKey = os.Getenv("APPINSIGHTS_KEY")
var challengeInsightsKey = os.Getenv("CHALLENGEAPPINSIGHTS_KEY")
var mongoURL = os.Getenv("MONGOURL")
var amqpURL = os.Getenv("AMQPURL")
var teamName = os.Getenv("TEAMNAME")
var mongoPoolLimit = 25

// MongoDB variables
var mongoDBSession *mgo.Session
var mongoDBSessionError error

// MongoDB database and collection names
var mongoDatabaseName = "k8orders"
var mongoCollectionName = "orders"
var mongoCollectionShardKey = "partition"

// AMQP 0.9.1 variables
var amqp091Client *amqp091.Connection
var amqp091Channel *amqp091.Channel
var amqp091Queue amqp091.Queue

// AMQP 1.0 variables
var amqp10Client *amqp10.Client
var amqp10Session *amqp10.Session
var amqpSender *amqp10.Sender
var serivceBusName string

// Application Insights telemetry clients
var challengeTelemetryClient appinsights.TelemetryClient
var customTelemetryClient appinsights.TelemetryClient

// For tracking and code branching purposes
var isCosmosDb = strings.Contains(mongoURL, "documents.azure.com")
var isServiceBus = strings.Contains(amqpURL, "servicebus.windows.net")
var db string        // CosmosDB or MongoDB?
var queueType string // ServiceBus or RabbitMQ

func TrackInitialOrder(order Order) {
	eventTelemetry := appinsights.NewEventTelemetry("Initial order")
	eventTelemetry.Properties["team"] = teamName
	eventTelemetry.Properties["sequence"] = "0"
	eventTelemetry.Properties["type"] = "http"
	eventTelemetry.Properties["service"] = "CaptureOrder"
	eventTelemetry.Properties["orderId"] = order.OrderID
	challengeTelemetryClient.Track(eventTelemetry)
	if customTelemetryClient != nil {
		customTelemetryClient.Track(eventTelemetry)
	}
}

// AddOrderToMongoDB Adds the order to MongoDB/CosmosDB
func AddOrderToMongoDB(order Order) (Order, error) {
	success := false
	startTime := time.Now()

	// Use the existing mongoDBSessionCopy
	mongoDBSessionCopy := mongoDBSession.Copy()
	defer mongoDBSessionCopy.Close()

	log.Println("Team " + teamName)

	// Select a random partition
	rand.Seed(time.Now().UnixNano())
	partitionKey := strconv.Itoa(random(0, 11))
	order.Partition = fmt.Sprintf("partition-%s", partitionKey)

	NewOrderID := bson.NewObjectId()
	order.OrderID = NewOrderID.Hex()

	order.Status = "Open"
	if order.Source == "" || order.Source == "string" {
		order.Source = os.Getenv("SOURCE")
	}

	log.Print("Inserting into MongoDB URL: ", mongoURL, " CosmosDB: ", isCosmosDb)

	// insert Document in collection
	mongoDBCollection := mongoDBSessionCopy.DB(mongoDatabaseName).C(mongoCollectionName)
	mongoDBSessionError = mongoDBCollection.Insert(order)
	log.Println("Inserted order:", order)

	if mongoDBSessionError != nil {
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(mongoDBSessionError)
		}
		log.Println("Problem inserting data: ", mongoDBSessionError)
		log.Println("_id:", order)
	} else {
		success = true
	}

	endTime := time.Now()

	if success {
		// Track the event for the challenge purposes
		eventTelemetry := appinsights.NewEventTelemetry("CaptureOrder to " + db)
		eventTelemetry.Properties["team"] = teamName
		eventTelemetry.Properties["sequence"] = "1"
		eventTelemetry.Properties["type"] = db
		eventTelemetry.Properties["service"] = "CaptureOrder"
		eventTelemetry.Properties["orderId"] = order.OrderID
		challengeTelemetryClient.Track(eventTelemetry)
	}

	// Track the dependency, if the team provided an Application Insights key, let's track that dependency
	if customTelemetryClient != nil {
		if isCosmosDb {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"CosmosDB",
				"MongoDB",
				mongoURL,
				success)
			dependency.Data = "Insert order"

			if mongoDBSessionError != nil {
				dependency.ResultCode = mongoDBSessionError.Error()
			}

			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.Track(dependency)
		} else {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"MongoDB",
				"MongoDB",
				mongoURL,
				success)
			dependency.Data = "Insert order"

			if mongoDBSessionError != nil {
				dependency.ResultCode = mongoDBSessionError.Error()
			}

			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.Track(dependency)
		}
	}

	return order, mongoDBSessionError
}

// AddOrderToAMQP Adds the order to AMQP (EventHub/RabbitMQ)
func AddOrderToAMQP(order Order) {
	if isServiceBus {
		addOrderToAMQP10(order)
	} else {
		addOrderToAMQP091(order)
	}
}

//// BEGIN: NON EXPORTED FUNCTIONS
func init() {

	rand.Seed(time.Now().UnixNano())

	// Validate environment variables
	validateVariable(customInsightsKey, "APPINSIGHTS_KEY")
	validateVariable(challengeInsightsKey, "CHALLENGEAPPINSIGHTS_KEY")
	validateVariable(mongoURL, "MONGOURL")
	validateVariable(amqpURL, "AMQPURL")
	validateVariable(teamName, "TEAMNAME")

	var mongoPoolLimitEnv = os.Getenv("MONGOPOOL_LIMIT")
	if mongoPoolLimitEnv != "" {
		if limit, err := strconv.Atoi(mongoPoolLimitEnv); err == nil {
			mongoPoolLimit = limit
		}
	}
	log.Printf("MongoDB pool limit set to %v. You can override by setting the MONGOPOOL_LIMIT environment variable.", mongoPoolLimit)

	// Initialize the Application Insights telemtry client(s)
	challengeTelemetryClient = appinsights.NewTelemetryClient(challengeInsightsKey)
	challengeTelemetryClient.Context().Tags.Cloud().SetRole("captureorder_golang")

	if customInsightsKey != "" {
		customTelemetryClient = appinsights.NewTelemetryClient(customInsightsKey)

		// Set role instance name globally -- this is usually the
		// name of the service submitting the telemetry
		customTelemetryClient.Context().Tags.Cloud().SetRole("captureorder_golang")
	}

	// Initialize the MongoDB client
	initMongo()

	// Initialize the AMQP client
	initAMQP()
}

// Logs out value of a variable
func validateVariable(value string, envName string) {
	if len(value) == 0 {
		log.Printf("The environment variable %s has not been set", envName)
	} else {
		log.Printf("The environment variable %s is %s", envName, value)
	}
}

func initMongoDial() (success bool, mErr error) {
	url, err := url.Parse(mongoURL)
	if err != nil {
		// If the team provided an Application Insights key, let's track that exception
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
		log.Fatal(fmt.Sprintf("Problem parsing Mongo URL %s: ", url), err)
	}

	if isCosmosDb {
		log.Println("Using CosmosDB")
		db = "CosmosDB"

	} else {
		log.Println("Using MongoDB")
		db = "MongoDB"
	}

	// Parse the connection string to extract components because the MongoDB driver is peculiar
	var dialInfo *mgo.DialInfo
	mongoUsername := ""
	mongoPassword := ""
	if url.User != nil {
		mongoUsername = url.User.Username()
		mongoPassword, _ = url.User.Password()
	}
	mongoHost := url.Host
	mongoDatabase := mongoDatabaseName // can be anything
	mongoSSL := strings.Contains(url.RawQuery, "ssl=true")

	log.Printf("\tUsername: %s", mongoUsername)
	log.Printf("\tPassword: %s", mongoPassword)
	log.Printf("\tHost: %s", mongoHost)
	log.Printf("\tDatabase: %s", mongoDatabase)
	log.Printf("\tSSL: %t", mongoSSL)

	if mongoSSL {
		dialInfo = &mgo.DialInfo{
			Addrs:    []string{mongoHost},
			Timeout:  10 * time.Second,
			Database: mongoDatabase, // It can be anything
			Username: mongoUsername, // Username
			Password: mongoPassword, // Password
			DialServer: func(addr *mgo.ServerAddr) (net.Conn, error) {
				return tls.Dial("tcp", addr.String(), &tls.Config{})
			},
		}
	} else {
		dialInfo = &mgo.DialInfo{
			Addrs:    []string{mongoHost},
			Timeout:  10 * time.Second,
			Database: mongoDatabase, // It can be anything
			Username: mongoUsername, // Username
			Password: mongoPassword, // Password
		}
	}

	// Create a mongoDBSession which maintains a pool of socket connections
	// to our MongoDB.
	success = false
	startTime := time.Now()

	log.Println("Attempting to connect to MongoDB")
	mongoDBSession, mongoDBSessionError = mgo.DialWithInfo(dialInfo)
	if mongoDBSessionError != nil {
		log.Println(fmt.Sprintf("Can't connect to mongo at [%s], go error: ", mongoURL), mongoDBSessionError)
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(mongoDBSessionError)
		}
		mErr = mongoDBSessionError
	} else {
		success = true
		log.Println("\tConnected")
	}

	mongoDBSession.SetMode(mgo.Monotonic, true)

	// Limit connection pool to avoid running into Request Rate Too Large on CosmosDB
	mongoDBSession.SetPoolLimit(mongoPoolLimit)

	endTime := time.Now()

	// Track the dependency, if the team provided an Application Insights key, let's track that dependency
	if customTelemetryClient != nil {
		if isCosmosDb {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"CosmosDB",
				"MongoDB",
				mongoURL,
				success)
			dependency.Data = "Create session"

			if mongoDBSessionError != nil {
				dependency.ResultCode = mongoDBSessionError.Error()
			}

			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.TrackException(mongoDBSessionError)
			customTelemetryClient.Track(dependency)
		} else {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"MongoDB",
				"MongoDB",
				mongoURL,
				success)
			dependency.Data = "Create session"

			if mongoDBSessionError != nil {
				dependency.ResultCode = mongoDBSessionError.Error()
			}

			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.TrackException(mongoDBSessionError)
			customTelemetryClient.Track(dependency)
		}
	}
	return
}

// Initialize the MongoDB client
func initMongo() {

	success, err := initMongoDial()
	if !success {
		os.Exit(1)
	}

	mongoDBSessionCopy := mongoDBSession.Copy()
	defer mongoDBSessionCopy.Close()

	// SetSafe changes the mongoDBSessionCopy safety mode.
	// If the safe parameter is nil, the mongoDBSessionCopy is put in unsafe mode, and writes become fire-and-forget,
	// without error checking. The unsafe mode is faster since operations won't hold on waiting for a confirmation.
	// http://godoc.org/labix.org/v2/mgo#Session.SetMode.
	mongoDBSessionCopy.SetSafe(nil)

	// Create a sharded collection and retrieve it
	result := bson.M{}
	err = mongoDBSessionCopy.DB(mongoDatabaseName).Run(
		bson.D{
			{
				"shardCollection",
				fmt.Sprintf("%s.%s", mongoDatabaseName, mongoCollectionName),
			},
			{
				"key",
				bson.M{
					mongoCollectionShardKey: "hashed",
				},
			},
		}, &result)

	if err != nil {
		trackException(err)
		// The collection is most likely created and already sharded. I couldn't find a more elegant way to check this.
		log.Println("Could not create/re-create sharded MongoDB collection. Either collection is already sharded or sharding is not supported. You can ignore this error: ", err)
	} else {
		log.Println("Created MongoDB collection: ")
		log.Println(result)
	}
}

// Initalize AMQP by figuring out where we are running
func initAMQP() {
	url, err := url.Parse(amqpURL)
	if err != nil {
		// If the team provided an Application Insights key, let's track that exception
		trackException(err)
		log.Fatal(fmt.Sprintf("Problem parsing AMQP Host %s. Make sure you URL Encoded your policy/password.", url), err)
	}

	// Figure out if we're running on ServiceBus or elsewhere
	if isServiceBus {
		log.Println("Using ServiceBus")
		queueType = "ServiceBus"

		// Parse the ServiceBus (last part of the url)
		serivceBusName = url.Path
		initAMQP10()
	} else {
		log.Println("Using RabbitMQ")
		queueType = "RabbitMQ"
		initAMQP091()
	}
	log.Println("\tAMQP URL: " + amqpURL)
	log.Println("** READY TO TAKE ORDERS **")
}

func initAMQP091() {
	log.Println("Attempting to connect to RabbitMQ")
	// Try to establish the connection to AMQP
	// with retry logic
	err := try.Do(func(attempt int) (bool, error) {
		var err error

		amqp091Client, err = amqp091.Dial(amqpURL)
		if err != nil {
			// If the team provided an Application Insights key, let's track that exception
			if customTelemetryClient != nil {
				customTelemetryClient.TrackException(err)
			}
		}

		if err != nil {
			log.Println("Error connecting to Rabbit instance. Will retry in 5 seconds:", err)
			time.Sleep(5 * time.Second) // wait
		}
		return attempt < 3, err
	})

	// If we still can't connect
	if err != nil {
		log.Println("Couldn't connect to Rabbit after 3 retries:", err)
	} else {
		log.Println("\tConnected to RabbitMQ. Establishing Channel and Queue")

		// Otherwise, let's continue and establish the channel and queue
		amqp091Channel, err = amqp091Client.Channel()
		if err != nil {
			// If the team provided an Application Insights key, let's track that exception
			if customTelemetryClient != nil {
				customTelemetryClient.TrackException(err)
			}
		}

		amqp091Queue, err = amqp091Channel.QueueDeclare(
			"order", // name
			true,    // durable
			false,   // delete when unused
			false,   // exclusive
			false,   // no-wait
			nil,     // arguments
		)
	}
}

func initAMQP10() {

	// Try to establish the connection to AMQP
	// with retry logic
	err := try.Do(func(attempt int) (bool, error) {
		var err error

		log.Println("Attempting to connect to ServiceBus")
		amqp10Client, err = amqp10.Dial(amqpURL)
		if err != nil {
			// If the team provided an Application Insights key, let's track that exception
			trackException(err)
		}
		//defer amqp10Client.Close()

		// Open a session if we managed to get an amqpClient
		log.Println("\tConnected to ServiceBus")
		if amqp10Client != nil {
			log.Println("\tCreating a new AMQP session")
			amqp10Session, err = amqp10Client.NewSession()
			if err != nil {
				// If the team provided an Application Insights key, let's track that exception
				trackException(err)
				log.Fatal("\t\tCreating AMQP session: ", err)
			}
		}

		// Create a sender
		log.Println("\tCreating AMQP sender")
		amqpSender, err = amqp10Session.NewSender(
			amqp10.LinkTargetAddress(serivceBusName),
		)
		if err != nil {
			// If the team provided an Application Insights key, let's track that exception
			if customTelemetryClient != nil {
				customTelemetryClient.TrackException(err)
			}
			log.Fatal("\t\tCreating sender link: ", err)
		}

		if err != nil {
			log.Println("Error connecting to ServiceBus instance. Will retry in 5 seconds:", err)
			time.Sleep(5 * time.Second) // wait
		}
		return attempt < 3, err
	})

	// If we still can't connect
	if err != nil {
		log.Println("Couldn't connect to ServiceBus after 3 retries:", err)
	}
}

// addOrderToAMQP091 Adds the order to AMQP 0.9.1
func addOrderToAMQP091(order Order) {
	if amqp091Channel == nil {
		log.Println("Skipping AMQP. It is either not configured or improperly configured")
	} else {
		success := false
		startTime := time.Now()
		body := fmt.Sprintf("{\"order\": \"%s\", \"source\": \"%s\"}", order.OrderID, teamName)

		// Send message
		err := amqp091Channel.Publish(
			"",                // exchange
			amqp091Queue.Name, // routing key
			false,             // mandatory
			false,             // immediate
			amqp091.Publishing{
				DeliveryMode: amqp091.Persistent,
				ContentType:  "application/json",
				Body:         []byte(body),
			})
		if err != nil {
			// If the team provided an Application Insights key, let's track that exception
			trackException(err)
			log.Println("Sending message:", err)
		} else {
			success = true
		}

		endTime := time.Now()

		if success {
			// Track the event for the challenge purposes
			eventTelemetry := appinsights.NewEventTelemetry("SendOrder to RabbitMQ")
			eventTelemetry.Properties["team"] = teamName
			eventTelemetry.Properties["sequence"] = "2"
			eventTelemetry.Properties["type"] = "rabbitmq"
			eventTelemetry.Properties["service"] = "CaptureOrder"
			eventTelemetry.Properties["orderId"] = order.OrderID
			challengeTelemetryClient.Track(eventTelemetry)
			if customTelemetryClient != nil {
				customTelemetryClient.Track(eventTelemetry)
			}
		}

		// Track the dependency, if the team provided an Application Insights key, let's track that dependency
		if customTelemetryClient != nil {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"RabbitMQ",
				"AMQP",
				amqpURL,
				success)
			dependency.Data = "Send message"

			if err != nil {
				dependency.ResultCode = err.Error()
			}

			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.Track(dependency)
		}

		log.Printf("Sent to AMQP 0.9.1 (RabbitMQ) - %t, %s: %s", success, amqpURL, body)
	}
}

// addOrderToAMQP10 Adds the order to AMQP 1.0 (sends to the Default ConsumerGroup)
func addOrderToAMQP10(order Order) {
	if amqp10Client == nil {
		log.Println("Skipping AMQP. It is either not configured or improperly configured")
	} else {
		// Only run this part if AMQP is configured
		success := false
		var err error
		startTime := time.Now()
		body := fmt.Sprintf("{\"order\": \"%s\", \"source\": \"%s\"}", order.OrderID, teamName)

		// Get an empty context
		amqp10Context := context.Background()

		log.Printf("AMQP URL: %s, Target: %s", amqpURL, serivceBusName)

		// Prepare the context to timeout in 5 seconds
		amqp10Context, cancel := context.WithTimeout(amqp10Context, 5*time.Second)

		// Send with retry logic (in case we get a amqp.DetachError)
		err = try.Do(func(attempt int) (bool, error) {
			var err error

			log.Println("Attempting to send the AMQP message: ", body)
			err = amqpSender.Send(amqp10Context, amqp10.NewMessage([]byte(body)))
			if err != nil {
				trackException(err)
				initAMQP10()
			}
			return attempt < 3, err
		})

		// Now check after possible retries if the message was sent
		success = (err == nil)

		// Cancel the context and close the sender
		cancel()
		//sender.Close()

		endTime := time.Now()

		if success {
			// Track the event for the challenge purposes
			eventTelemetry := appinsights.NewEventTelemetry("SendOrder to SerivceBus")
			eventTelemetry.Properties["team"] = teamName
			eventTelemetry.Properties["sequence"] = "2"
			eventTelemetry.Properties["type"] = "servicebus"
			eventTelemetry.Properties["service"] = "CaptureOrder"
			eventTelemetry.Properties["orderId"] = order.OrderID
			challengeTelemetryClient.Track(eventTelemetry)
		}

		// Track the dependency, if the team provided an Application Insights key, let's track that dependency
		if customTelemetryClient != nil {
			dependency := appinsights.NewRemoteDependencyTelemetry(
				"ServiceBus",
				"AMQP",
				amqpURL,
				success)
			dependency.Data = "Send message"

			if err != nil {
				dependency.ResultCode = err.Error()
			}

			dependency.MarkTime(startTime, endTime)
			customTelemetryClient.Track(dependency)
		}

		log.Printf("Sent to AMQP 1.0 (ServiceBus) - %t, %s: %s", success, amqpURL, body)
	}
}

func trackException(err error) {
	if err != nil {
		log.Println(err)
		challengeTelemetryClient.TrackException(err)
		if customTelemetryClient != nil {
			customTelemetryClient.TrackException(err)
		}
	}
}

// random: Generates a random number
func random(min int, max int) int {
	return rand.Intn(max-min) + min
}

//// END: NON EXPORTED FUNCTIONS
