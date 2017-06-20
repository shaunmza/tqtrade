package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/websocket"

	"github.com/shaunmza/coinmarketcap"
	"github.com/shaunmza/tradeqwik"
	"github.com/shaunmza/tradeqwik/trading"
)

type price struct {
	Base    string
	Counter string
	Targets targets
}

//Config struct
type config struct {
	VivaTargetPrice        float64
	TradeQwikTradesRefresh int
	TrackCoins             []*coinConfig
}

type coinConfig struct {
	CoinMarketCapID string
	TargetSpread    float64
	Base            string
	Counter         string
	PriceTarget     string
	Tiers           *tiers
}

type tiers struct {
	Buy  []*tier
	Sell []*tier
}

type tier struct {
	Target float64
	Amount float64
}

type targets struct {
	Buy  []target
	Sell []target
}

type target struct {
	Price  float64
	Amount float64
}

type listeners struct {
	writers []chan string
}

type configEdit struct {
	Config *config
	Error  error
}

var ct *coinmarketcap.Ticker
var c *config
var lstn listeners

var cRefreshPeriod int

var oChan chan *tradeqwik.OpenTrades
var cChan chan *coinmarketcap.Ticker
var mChan chan string

var priceTargets map[string]price
var openTrades map[string]*tradeqwik.OpenTrades

var wSocket *websocket.Conn

var templates = template.New("")

func main() {
	ch := make([]chan string, 0)
	lstn = listeners{ch}

	c = loadConfig("config.json")
	fmt.Println(c)

	// Put your API key in an environment variable
	k := os.Getenv("TQAPIKEY")
	trading.Init(k)

	// Initialise these, if you add more currencies, change the 1 to whatever
	priceTargets = make(map[string]price, 1)
	openTrades = make(map[string]*tradeqwik.OpenTrades, 1)

	t := make([]string, 0)
	for _, pair := range c.TrackCoins {
		t = append(t, pair.CoinMarketCapID)
	}

	// Coinmarketcap endpoints are updated every 5 minutes, se we use that here
	period := 120 //60 * 5
	ticker := time.NewTicker(time.Second * time.Duration(period))

	// Because we are impatient, call it now
	r, err := coinmarketcap.GetData(t)

	// If this is not nil then we encountered a problem, use this to determine
	// what to do next.
	// LastUpdate can be used to determine how stale the data is
	if err != nil {
		fmt.Printf("Error! %s, Last Updated: %s\n", err, r.LastUpdate)
	}

	mapCoins(r)

	http.Handle("/ws", websocket.Handler(wsHandler))
	http.Handle("/css/", http.StripPrefix("/css/", http.FileServer(http.Dir("static/css"))))
	http.Handle("/config", http.HandlerFunc(manageConfig))
	http.Handle("/", http.FileServer(http.Dir("static/html")))
	go func() {
		http.ListenAndServe(fmt.Sprintf(":%d", 4050), nil)
	}()

	// Get latest prices
	r, err = coinmarketcap.GetData(t)

	// If this is not nil then we encountered a problem, use this to determine
	// what to do next.
	// LastUpdate can be used to determine how stale the data is
	if err != nil {
		fmt.Printf("Error! %s, Last Updated: %s\n", err, r.LastUpdate)
	}

	// Set our prices
	mapCoins(r)
	setWalls()
	// Infinite loop so we keep getting prices
	for _ = range ticker.C {
		// Get latest prices
		r, err = coinmarketcap.GetData(t)

		// If this is not nil then we encountered a problem, use this to determine
		// what to do next.
		// LastUpdate can be used to determine how stale the data is
		if err != nil {
			fmt.Printf("Error! %s, Last Updated: %s\n", err, r.LastUpdate)
		}

		// Set our prices
		mapCoins(r)

		setWalls()

	}
}

func setWalls() {
	broadcast(fmt.Sprintln("Going to set buy / sell walls"))
	fmt.Println("Going to set buy / sell walls " + string(time.Now().Format("15:04")))

	// get balances
	balance, err := trading.GetBalance()
	broadcast(fmt.Sprintf("Balance is %+v", balance))
	if err != nil {
		broadcast(fmt.Sprintln("Not setting walls, got an error from TQ"))
		broadcast(err.Error())
		return
	}

	// get my open trades for each watched pair
	openTrades, err := trading.GetPending()
	if err != nil {
		broadcast(fmt.Sprintln("Not setting walls, got an error from TQ"))
		broadcast(err.Error())
		return
	}

	// get sell / buy wall levels
	for _, pair := range c.TrackCoins {
		for _, t := range openTrades.Trades {
			// Simplest, cancel them all
			if t.Base == pair.Base && t.Counter == pair.Counter {
				trading.Cancel(t.ID)
			}
		}

		// get targets for pair
		for _, b := range priceTargets[pair.PriceTarget].Targets.Buy {

			if balance.Currencies[pair.PriceTarget].Amount < b.Amount*b.Price {
				broadcast(fmt.Sprintf("Insufficient balance (%f) cannot Buy %f %s/%s @ %f ",
					balance.Currencies[pair.PriceTarget].Amount, b.Amount, pair.Base, pair.Counter, b.Price))
				continue
			}

			// Now recreate
			broadcast(fmt.Sprintf("Going to Buy %f %s/%s @ %f ", b.Amount, pair.Base, pair.Counter, b.Price))
			a, err := trading.Buy(pair.Base, pair.Counter, b.Amount, b.Price)
			broadcast(fmt.Sprintf("Buying response %+v", a))
			if err != nil {
				broadcast(fmt.Sprintln("Failed setting buy walls, got an error from TQ"))
				broadcast(err.Error())
				return
			}
		}

		for _, s := range priceTargets[pair.PriceTarget].Targets.Sell {
			if balance.Currencies[pair.PriceTarget].Amount < s.Amount*s.Price {
				broadcast(fmt.Sprintf("Insufficient balance (%f) cannot Sell %f %s/%s @ %f ",
					balance.Currencies[pair.PriceTarget].Amount, s.Amount, pair.Base, pair.Counter, s.Price))
				continue
			}
			// Now recreate
			broadcast(fmt.Sprintf("Going to Sell %f %s/%s @ %f ", s.Amount, pair.Base, pair.Counter, s.Price))
			a, err := trading.Buy(pair.Base, pair.Counter, s.Amount, s.Price)
			broadcast(fmt.Sprintf("Selling response %+v", a))
			if err != nil {
				broadcast(fmt.Sprintln("Failed setting buy walls, got an error from TQ"))
				broadcast(err.Error())
				return
			}
		}

	}
}

func mapCoins(ticker coinmarketcap.Ticker) {
	broadcast(fmt.Sprintln("Mapping coins"))
	fmt.Println("Mapping coins " + string(time.Now().Format("15:04")))

	ts := targets{}
	for _, coin := range ticker.Coins {
		for _, tc := range c.TrackCoins {
			if tc.CoinMarketCapID == coin.ID {
				var btgs []target
				for _, tr := range tc.Tiers.Buy {
					price := coin.PriceUsd/c.VivaTargetPrice + (coin.PriceUsd / c.VivaTargetPrice * tr.Target / 100)
					t := target{Price: (1 / price), Amount: tr.Amount}
					btgs = append(btgs, t)
				}
				ts.Buy = btgs

				var stgs []target
				for _, tr := range tc.Tiers.Sell {
					price := coin.PriceUsd/c.VivaTargetPrice - (coin.PriceUsd / c.VivaTargetPrice * tr.Target / 100)
					t := target{Price: (1 / price), Amount: tr.Amount}
					stgs = append(stgs, t)
				}
				ts.Sell = stgs
				continue
			}
		}

		switch coin.ID {
		case "bitcoin":
			fmt.Printf("Bitcoin price: %f, USD price: %f, Target VIVA price: %+v\n\n", coin.PriceBtc, coin.PriceUsd, ts)
			broadcast(fmt.Sprintf("Bitcoin BTC price: %f, USD price: %f<br>Target VIVA prices: %+v<br>", coin.PriceBtc, coin.PriceUsd, ts))
			priceTargets["BTC"] = price{Base: "VIVA", Counter: "BTC", Targets: ts}
		case "litecoin":
			fmt.Printf("Litecoin price: %f, Target VIVA price: %f\n\n", coin.PriceUsd, ts)
			broadcast(fmt.Sprintf("Litecoin BTC price: %f, USD price: %f<br>Target VIVA price: %+v<br>", coin.PriceBtc, coin.PriceUsd, ts))
			priceTargets["LTC"] = price{Base: "VIVA", Counter: "LTC", Targets: ts}
		case "steem":
			fmt.Printf("Steem price: %f, Target VIVA price: %f\n\n", coin.PriceUsd, ts)
			broadcast(fmt.Sprintf("Steem BTC price: %f, USD price: %f<br>Target VIVA price: %+v<br>", coin.PriceBtc, coin.PriceUsd, ts))
			priceTargets["STEEM"] = price{Base: "VIVA", Counter: "STEEM", Targets: ts}
		case "golos":
			fmt.Printf("Steem price: %f, Target VIVA price: %f\n\n", coin.PriceUsd, ts)
			broadcast(fmt.Sprintf("Golos BTC price: %f, USD price: %f<br>Target VIVA price: %+v<br>", coin.PriceBtc, coin.PriceUsd, ts))
			priceTargets["GOLOS"] = price{Base: "VIVA", Counter: "STEEM", Targets: ts}
		case "ethereum":
			fmt.Printf("Steem price: %f, Target VIVA price: %f\n\n", coin.PriceUsd, ts)
			broadcast(fmt.Sprintf("Ethereum BTC price: %f, USD price: %f<br>Target VIVA price: %+v<br>", coin.PriceBtc, coin.PriceUsd, ts))
			priceTargets["ETH"] = price{Base: "VIVA", Counter: "STEEM", Targets: ts}
		}
	}

}

func loadConfig(cFile string) *config {
	f, err := ioutil.ReadFile(cFile)

	if err != nil {
		panic(fmt.Sprintf("Failed to open config file: %v\n", err))
	}

	c := &config{}
	err = json.Unmarshal(f, &c)

	if err != nil {
		panic(fmt.Sprintf("Could not open config file: %v\n", err))
	}

	return c
}

func wsHandler(ws *websocket.Conn) {
	w := bufio.NewWriter(ws)
	messages := make(chan string)
	defer close(messages)
	lstn.writers = append(lstn.writers, messages)

	websocket.Message.Send(ws, "connected")
	s, _ := json.Marshal(c)
	websocket.Message.Send(ws, fmt.Sprintf("Config is<br>%s", string(s)))
	for {
		select {
		case msg := <-messages:
			w.WriteString(string(time.Now().Format("15:04")) + " " + msg)
			w.Flush()

		}
	}

}

func broadcast(data string) {
	go func() {
		for _, cl := range lstn.writers {
			fmt.Println("Writing to listener " + string(time.Now().Format("15:04")))
			cl <- data
		}
	}()
}

func manageConfig(w http.ResponseWriter, r *http.Request) {
	fmt.Println("method:", r.Method) //get request method
	var tC = &config{}
	var cUse = configEdit{}
	var err error

	if r.Method == "POST" {
		r.ParseForm()
		//fmt.Printf("Config: %+v\n", r.Form)
		tC, err = saveConfig(r.Form)
		cUse = configEdit{Config: tC, Error: err}
	} else {
		cUse = configEdit{Config: c}
	}

	//p, _ := json.Marshal(c)
	//t := template.New("templates/config.html")      //create a new template
	t, err := template.ParseFiles("templates/config.html") //open and parse a template text file
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	err = t.Execute(w, cUse)

	/*err = t.ExecuteTemplate(w, "templates/config.html", p)*/
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	} /**/
}

func saveConfig(formData url.Values) (*config, error) {

	for key, values := range formData {
		fmt.Printf("%+v : %+v\n", key, values)

	}
	var j = []byte(formData["jsonConfig"][0])

	cTest := &config{}
	err := json.Unmarshal(j, &cTest)

	if err != nil {
		return cTest, err
	}

	err = filePutContents("config.json", j)

	if err == nil {
		c = cTest
		broadcast("Config updated")
	} else {
		fmt.Println(err)
	}

	return cTest, err
}

func filePutContents(filename string, content []byte) error {
	err := ioutil.WriteFile(filename, content, os.ModePerm)

	return err
}
