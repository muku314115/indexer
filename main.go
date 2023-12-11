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
	"io/ioutil"
	"sync"

	rpcclient "github.com/tendermint/tendermint/rpc/client"
	tmhttp "github.com/tendermint/tendermint/rpc/client/http"
	libclient "github.com/tendermint/tendermint/rpc/jsonrpc/client"

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
		Addr:              ":8090",
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
		client, err := NewRPCClient("https://terra-classic-rpc.publicnode.com:443", 30*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		interfaceRegistry := cdctypes.NewInterfaceRegistry()
		authtypes.RegisterInterfaces(interfaceRegistry)
		marshaler := codec.NewProtoCodec(interfaceRegistry)

		var mu sync.Mutex
		amt := math.NewInt(0)

		var next []byte

		// Define a function to process accounts
		processAccounts := func(accounts []*cdctypes.Any) {
			var wg sync.WaitGroup
			defer wg.Wait()

			for _, i := range accounts {
				if i == nil {
					continue
				}

				wg.Add(1)
				go func(i *cdctypes.Any) {
					defer wg.Done()

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

					if len(txsList.Txs) > 0 && !isTimestampWithinAYear(txsList.Txs[0].Timestamp) {
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

					mu.Lock()
					defer mu.Unlock()

					for _, bal := range bankQueryResponse.Balances {
						if bal.GetDenom() == mux.Vars(req)["denom"] {
							amt.Add(bal.Amount)
						}
					}
				}(i)
			}
		}

		// Fetch accounts in batches
		for {
			query := authtypes.QueryAccountsRequest{Pagination: &query2.PageRequest{Limit: 10000, Key: next}}
			bytes := marshaler.MustMarshal(&query)

			abciquery, err := client.ABCIQueryWithOptions(
				context.Background(),
				"/cosmos.auth.v1beta1.Query/Accounts",
				bytes,
				rpcclient.ABCIQueryOptions{},
			)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			response := authtypes.QueryAccountsResponse{}
			marshaler.Unmarshal(abciquery.Response.Value, &response)

			next = response.Pagination.NextKey

			// Process accounts in parallel
			processAccounts(response.Accounts)

			fmt.Println(next)

			if next == nil {
				break
			}
		}

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
