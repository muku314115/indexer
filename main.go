package main

import (
	"context"
	"cosmossdk.io/math"
	"encoding/json"
	"fmt"
	"github.com/cosmos/cosmos-sdk/codec"
	cdctypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/types"
	query2 "github.com/cosmos/cosmos-sdk/types/query"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/gorilla/mux"
	rpcclient "github.com/tendermint/tendermint/rpc/client"
	tmhttp "github.com/tendermint/tendermint/rpc/client/http"
	libclient "github.com/tendermint/tendermint/rpc/jsonrpc/client"
	"io/ioutil"
	"sync"

	"net/http"
	"time"
)

const (
	FCD_URI = "https://terra-classic-fcd.publicnode.com/v1/txs?account="
	Year    = 365 * 24 * time.Hour
)

type TxsListResponse struct {
	Next  int  `json:"next"`
	Limit int  `json:"limit"`
	Txs   []Tx `json:"txs"`
}

type Tx struct {
	ID        int    `json:"id"`
	ChainID   string `json:"chainId"`
	Tx        any    `json:"tx"`
	Logs      any    `json:"logs"`
	Height    string `json:"height"`
	TxHash    string `json:"txhash"`
	RawLog    string `json:"raw_log"`
	GasUsed   string `json:"gas_used"`
	Timestamp string `json:"timestamp"`
	GasWanted string `json:"gas_wanted"`
}

func main() {
	config := types.GetConfig()
	config.SetBech32PrefixForAccount("terra", "terrapub")
	fmt.Println("starting service")

	r := mux.NewRouter()
	r.HandleFunc("/circulatingsupply/{denom}", getCirculatingSupply())

	server := &http.Server{
		Addr:              ":8000",
		Handler:           r,
		ReadHeaderTimeout: 10000 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("Error: %v\n", err.Error())
		return
	}
}

func getCirculatingSupply() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var wg sync.WaitGroup
		client, err := NewRPCClient("https://terra-classic-rpc.publicnode.com:443", 30*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		interfaceRegistry := cdctypes.NewInterfaceRegistry()
		authtypes.RegisterInterfaces(interfaceRegistry)
		marshaler := codec.NewProtoCodec(interfaceRegistry)

		amt := math.NewInt(0)

		// Define a function to process accounts
		processAccount := func(i *cdctypes.Any, resultsCh chan<- math.Int) {
			defer wg.Done()

			if i == nil {
				return
			}

			var acc types.AccountI
			if err := marshaler.InterfaceRegistry().UnpackAny(i, &acc); err != nil {
				return
			}

			resp, err := http.Get(fmt.Sprintf("%s%s", FCD_URI, acc.GetAddress().String()))
			if err != nil {
				return
			}
			defer resp.Body.Close()

			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return
			}

			txsList := &TxsListResponse{}
			if err := json.Unmarshal(body, txsList); err != nil {
				return
			}

			if len(txsList.Txs) == 0 || !isTimestampWithinAYear(txsList.Txs[0].Timestamp) {
				return
			}

			bankquery := banktypes.QueryAllBalancesRequest{Address: acc.GetAddress().String()}
			bytes := marshaler.MustMarshal(&bankquery)

			abciquery, err := client.ABCIQueryWithOptions(
				context.Background(),
				"/cosmos.bank.v1beta1.Query/AllBalances",
				bytes,
				rpcclient.ABCIQueryOptions{},
			)
			if err != nil {
				return
			}

			var bankQueryResponse banktypes.QueryAllBalancesResponse
			if err := marshaler.Unmarshal(abciquery.Response.Value, &bankQueryResponse); err != nil {
				return
			}

			for _, bal := range bankQueryResponse.Balances {
				if bal.GetDenom() == mux.Vars(req)["denom"] {
					resultsCh <- bal.Amount
				}
			}
		}

		offset := uint64(0)
		ch := make(chan *cdctypes.Any)
		safe := true
		processQuery := func(query authtypes.QueryAccountsRequest, ch chan<- *cdctypes.Any) {
			bytes := marshaler.MustMarshal(&query)

			abciquery, err := client.ABCIQueryWithOptions(
				context.Background(),
				"/cosmos.auth.v1beta1.Query/Accounts",
				bytes,
				rpcclient.ABCIQueryOptions{},
			)
			if err != nil {
				safe = false
				fmt.Println("encountered error: " + err.Error())
				return
			}
			response := authtypes.QueryAccountsResponse{}
			marshaler.Unmarshal(abciquery.Response.Value, &response)
			fmt.Println("Response")
			fmt.Println(response.String())
			// Send individual accounts to accCh
			for _, acc := range response.Accounts {
				ch <- acc
			}
		}

		for safe {
			query := authtypes.QueryAccountsRequest{Pagination: &query2.PageRequest{Limit: 10000, Offset: offset}}
			wg.Add(1)
			go processQuery(query, ch)
			offset += 10000
		}

		// Start a goroutine to close the results channel when all processing is done
		go func() {
			wg.Wait()
			close(ch)
			fmt.Println("Queries processed")
		}()

		// Use a channel to collect individual results
		resultsCh := make(chan math.Int)

		// Process accounts concurrently
		for i := range ch {
			wg.Add(1)
			go processAccount(i, resultsCh)
		}

		// Start a goroutine to close the results channel when all processing is done
		go func() {
			wg.Wait()
			close(resultsCh)
			fmt.Println("Results processed")
		}()

		// Collect individual results and sum them up
		for individualAmt := range resultsCh {
			//mu.Lock()
			amt.Add(individualAmt)
			//mu.Unlock()
		}

		// Print or use the final result (amt)
		fmt.Fprint(w, amt.String())
	}
}

func isTimestampWithinAYear(tsStr string) bool {
	// Parse the timestamp string into a time.Time value
	timestamp, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return false
	}

	// Calculate the duration between the parsed timestamp and the current time
	duration := time.Since(timestamp)

	// Check if the duration is less than a year (365 days)
	return duration < Year
}

func NewRPCClient(addr string, timeout time.Duration) (*tmhttp.HTTP, error) {
	httpClient, err := libclient.DefaultHTTPClient(addr)
	if err != nil {
		return nil, err
	}
	httpClient.Timeout = timeout
	rpcClient, err := tmhttp.NewWithClient(addr, httpClient)
	if err != nil {
		return nil, err
	}
	return rpcClient, nil
}
