/*
   Copyright 2018 the original author or authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containous/flaeg"
	"github.com/containous/staert"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gin-gonic/gin"
	"github.com/go-ble/ble"
	"github.com/gorilla/websocket"
	log "github.com/mgutz/logxi/v1"
	"github.com/sworisbreathing/go-ibbq/v2"
	"golang.org/x/sync/errgroup"
)

var logger = log.New("main")

var done = make(chan struct{})
var tempsChannel = make(chan []float64)
var batteryLevelChannel = make(chan []int)
var statusChannel = make(chan *ibbq.Status)
var shutdown = false

func main() {
	command := newCommand(run)
	s := staert.NewStaert(command)
	toml := staert.NewTomlSource("ibbq-websocket", []string{"."})
	f := flaeg.New(command, os.Args[1:])
	if _, err := f.Parse(command); err != nil {
		fmt.Println(err)
		os.Exit(-1)
	}
	s.AddSource(toml)
	s.AddSource(f)
	if _, err := s.LoadConfig(); err != nil {
		logger.Fatal(err.Error())
	}

	gin.SetMode(gin.ReleaseMode)
	if err := s.Run(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err.Error())
	}
	logger.Info("Exiting")
}

func run(config *Configuration) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registerInterruptHandler(cancel)
	router := gin.Default()
	var g errgroup.Group
	temps := []float64{}
	batteryLevel := 0
	status := ibbq.Disconnected
	router.GET("/temperatureData", func(c *gin.Context) {
		c.JSON(
			http.StatusOK,
			gin.H{
				"temperatures": temps,
			},
		)
	})
	router.GET("/batteryLevel", func(c *gin.Context) {
		c.JSON(
			http.StatusOK,
			gin.H{
				"batteryLevel": batteryLevel,
			},
		)
	})
	router.GET("/allData", func(c *gin.Context) {
		c.JSON(
			http.StatusOK,
			gin.H{
				"status":       status,
				"batteryLevel": batteryLevel,
				"temperatures": temps,
			},
		)
	})
	router.GET("/ws", func(c *gin.Context) {
		if err := handleWebsocket(c.Writer, c.Request); err != nil {
			logger.Error(err.Error())
		}
	})
	router.StaticFile("/", "client/index.html")
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", config.Port),
		Handler: router,
	}

	//mqtt.DEBUG = log.New(os.Stdout, "", 0)
	//mqtt.ERROR = log.New(os.Stdout, "", 0)
	opts := mqtt.NewClientOptions().AddBroker(config.MqttBroker).SetClientID("gotrivial")
	opts.SetKeepAlive(2 * time.Second)
	//opts.SetDefaultPublishHandler(f)
	opts.SetPingTimeout(1 * time.Second)

	mqttClient := mqtt.NewClient(opts)

	g.Go(func() error {
		for {
			select {
			case t := <-tempsChannel:
				if t == nil {
					logger.Info("temps channel closed")
					return nil
				}
				temps = t
				go updateWebsockets(status, batteryLevel, temps)
				go updateMqtt(mqttClient, status, batteryLevel, temps)
			case bl := <-batteryLevelChannel:
				if bl == nil {
					logger.Info("battery level channel closed")
					return nil
				}
				batteryLevel = bl[0]
				go updateWebsockets(status, batteryLevel, temps)
				go updateMqtt(mqttClient, status, batteryLevel, temps)
			case s := <-statusChannel:
				if s == nil {
					logger.Info("status channel closed")
					return nil
				} else if *s != ibbq.Connected {
					batteryLevel = 0
					temps = []float64{}
				}
				status = *s
				go updateWebsockets(status, batteryLevel, temps)
				go updateMqtt(mqttClient, status, batteryLevel, temps)
			case <-done:
				logger.Info("shutdown detected")
				close(tempsChannel)
				close(batteryLevelChannel)
				close(statusChannel)
				return nil
			}
		}
	})
	g.Go(func() error {
		for {
			if shutdown {
				logger.Info("shutdown detected")
				return nil
			}
			logger.Info("Connecting to ibbq")
			if bbq, err := startIbbq(ctx, cancel, config.IbbqConfiguration, tempsChannel, batteryLevelChannel, statusChannel); err != nil {
				logger.Error("error connecting", "err", err)
				if bbq != nil {
					bbq.Disconnect(true)
				}
				time.Sleep(30 * time.Second)
			}
		}
	})
	g.Go(func() error {
		logger.Info("Connecting to mqtt broker", "broker", config.MqttBroker)

		if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
			panic(token.Error())
			return token.Error()
		}

		return nil
	})
	g.Go(func() error {
		logger.Info("Starting websocket server", "port", config.Port)
		err := srv.ListenAndServe()
		logger.Info("server is done")
		return err
	})
	go func() {
		<-ctx.Done()
		logger.Info("shutting down server")
		sdc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		close(done)
		shutdown = true
		if err := srv.Shutdown(sdc); err != nil {
			logger.Fatal("Shutdown failed")
		}
		logger.Info("Server shut down")
	}()
	return g.Wait()
}

func startIbbq(ctx1 context.Context, cancel1 func(), config IbbqConfiguration, tempsChannel chan []float64, batteryLevelChannel chan []int, statusChannel chan *ibbq.Status) (*ibbq.Ibbq, error) {
	ctx, cancel := context.WithCancel(ble.WithSigHandler(ctx1, cancel1))
	defer cancel()
	var bbq ibbq.Ibbq
	var ibbqConfig ibbq.Configuration
	var err error
	if ibbqConfig, err = config.asConfig(); err != nil {
		return nil, err
	}
	disconnectedHandler := func() {
		logger.Info("Disconnected")
		cancel()
	}
	temperatureReceived := func(temps []float64) {
		tempsChannel <- temps
	}
	batteryLevelReceived := func(batteryLevel int) {
		batteryLevelChannel <- []int{batteryLevel}
	}
	statusUpdated := func(status ibbq.Status) {
		statusChannel <- &status
	}
	if bbq, err = ibbq.NewIbbq(ctx, ibbqConfig, disconnectedHandler, temperatureReceived, batteryLevelReceived, statusUpdated); err != nil {
		return nil, err
	}
	if err = bbq.Connect(); err != nil {
		return &bbq, err
	}
	logger.Info("Connected to ibbq")
	tempUnits := strings.ToLower(config.TemperatureUnits)
	if tempUnits == "c" || tempUnits == "celsius" {
		if err = bbq.ConfigureTemperatureCelsius(); err != nil {
			return &bbq, err
		}
	} else if tempUnits == "f" || tempUnits == "fahrenheit" {
		if err = bbq.ConfigureTemperatureFahrenheit(); err != nil {
			return &bbq, err
		}
	} else {
		err = errors.New("Unrecognized temperature units: " + config.TemperatureUnits)
		return &bbq, err
	}
	<-ctx.Done()
	return &bbq, nil
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

var connections = []*websocket.Conn{}

var connectionsMutex = &sync.RWMutex{}

func handleWebsocket(w http.ResponseWriter, r *http.Request) error {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	connectionsMutex.Lock()
	connections = append(connections, conn)
	logger.Debug("Connection added", "connections", connections)
	connectionsMutex.Unlock()
	return nil
}

func connectionClosed(conn *websocket.Conn) {
	logger.Debug("Connection closed", "conn", conn)
	connectionsMutex.Lock()
	for i, c := range connections {
		if c == conn {
			copy(connections[i:], connections[i+1:])
			connections[len(connections)-1] = nil
			connections = connections[:len(connections)-1]
		}
	}
	logger.Debug("Connection removed", "connections", connections)
	connectionsMutex.Unlock()
}

func updateMqtt(c *mqtt.Client, status ibbq.Status, batteryLevel int, temps []float64) {

	if token := c.Subscribe("go-mqtt/sample", 0, nil); token.Wait() && token.Error() != nil {
		fmt.Println(token.Error())
		os.Exit(1)
	}

	for i := 0; i < 5; i++ {
		text := fmt.Sprintf("this is msg #%d!", i)
		token := c.Publish("go-mqtt/sample", 0, false, text)
		token.Wait()
	}

	time.Sleep(6 * time.Second)

	if token := c.Unsubscribe("go-mqtt/sample"); token.Wait() && token.Error() != nil {
		fmt.Println(token.Error())
		os.Exit(1)
	}

	c.Disconnect(250)

	time.Sleep(1 * time.Second)
}

func updateWebsockets(status ibbq.Status, batteryLevel int, temps []float64) {
	connectionsMutex.RLock()
	for _, conn := range connections {
		go func(conn *websocket.Conn) {
			if err := conn.WriteJSON(
				gin.H{
					"status":       status,
					"batteryLevel": batteryLevel,
					"temps":        temps,
				},
			); err != nil {
				if isClosedError(err) {
					connectionClosed(conn)
				} else {
					logger.Error("Error writing to websocket", "err", err)
				}
			}
		}(conn)
	}
	connectionsMutex.RUnlock()
}

func isClosedError(err error) bool {
	logger.Debug(reflect.TypeOf(err).String())
	if websocket.IsUnexpectedCloseError(err) {
		return true
	}
	switch err.(type) {
	default:
		return false
	case syscall.Errno:
		if err.(syscall.Errno) == syscall.EPIPE {
			return true
		}
		return false
	case *net.OpError:
		err1 := err.(*net.OpError).Err
		return isClosedError(err1)
	case *os.SyscallError:
		err1 := err.(*os.SyscallError).Err
		return isClosedError(err1)
	}
}
