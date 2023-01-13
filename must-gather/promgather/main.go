package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type HttpClient struct {
	*http.Client
	headers map[string]string
	host    string
}

func (httpClient HttpClient) Get(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	for headerKey, headerValue := range httpClient.headers {
		req.Header.Set(headerKey, headerValue)
	}
	return httpClient.Do(req)
}

func main() {
	logFile, err := os.OpenFile("promgather.log", os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		log.Println(err)
		return
	}
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)

	log.Println("Starting: prometheus metrics gathering.")
	start := time.Now()

	// Get the required data for the http client.
	token, err := getCmdResult("oc", "whoami", "-t")
	if err != nil {
		log.Printf("Error fetching the auth token: %v\n", err)
		return
	}
	headers := make(map[string]string)
	headers["Authorization"] = "Bearer " + token
	host, err := getCmdResult("oc", "-n", "openshift-monitoring", "get", "route", "thanos-querier", "-ojsonpath={.spec.host}")
	if err != nil {
		log.Printf("Error fetching the api server host: %v\n", err)
		return
	}

	// Create the prometheus http api client.
	customTransport := http.DefaultTransport.(*http.Transport).Clone()
	customTransport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	customTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec
	customTransport.TLSHandshakeTimeout = 5 * time.Second
	httpClient := HttpClient{&http.Client{
		Transport: customTransport,
		Timeout:   10 * time.Second,
	}, headers, host}

	// Get the metrics' list.
	storageMetrics, err := getMetricList(httpClient)
	if err != nil {
		log.Printf("Error on fetching the metric list: %v\n", err)
		return
	}
	if len(storageMetrics) == 0 {
		log.Println("Error: prometheus storage metrics not found.")
		return
	}

	// Create the metrics folder.
	metricsFolder := "./prom-metrics"
	if err = os.RemoveAll(metricsFolder); err != nil {
		log.Printf("Error removing the previous metrics folder: %v\n", err)
		return
	}
	if err = os.Mkdir(metricsFolder, 0750); err != nil {
		log.Printf("Error creating the metrics folder: %v\n", err)
		return
	}

	// Fetch the metrics.
	var wg sync.WaitGroup
	for metricIndex := 0; metricIndex < len(storageMetrics); metricIndex++ {
		wg.Add(1)
		go getMetric(httpClient, &wg, storageMetrics[metricIndex], metricsFolder)
	}
	wg.Wait()
	elapsed := time.Since(start)
	log.Printf("Finished: prometheus metrics gathering. Time elapsed: %s\n", elapsed)
}

func getCmdResult(command string, arg ...string) (result string, err error) {
	cmd := exec.Command(command, arg...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	if err != nil {
		return result, err
	}
	return strings.Trim(out.String(), "\n"), nil
}

func getMetricList(httpClient HttpClient) (list []string, err error) {
	hostBaseUrl := "https://" + httpClient.host
	apiUrl, err := url.Parse(hostBaseUrl)
	if err != nil {
		log.Printf("Error parsing the host base url: %v\n", err)
		return list, err
	}
	apiPath, err := url.Parse("api/v1/label/__name__/values")
	if err != nil {
		log.Printf("Error parsing the api endpoint: %v\n", err)
		return list, err
	}
	apiUrl = apiUrl.ResolveReference(apiPath)
	query := apiUrl.Query()
	query.Set("start", fmt.Sprintf("%d", time.Now().Add(-time.Hour).Unix()))
	query.Set("end", fmt.Sprintf("%d", time.Now().Unix()))
	query.Set("match[]", "{__name__=~\"(ceph|Ceph|noobaa|NooBaa|ocs|odf).+\"}")
	apiUrl.RawQuery = query.Encode()

	response, err := httpClient.Get(apiUrl.String())
	if err != nil {
		log.Printf("Error fetching the metric list: %v\n", err)
		return list, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		log.Printf("Metric list fetch not OK: HTTP status code %v\n", response.Status)
		return list, err
	}

	var result struct {
		Data []string `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
		log.Printf("Error parsing the metric list result: %v\n", err)
		return list, err
	}
	return result.Data, nil
}

func getMetric(httpClient HttpClient, wg *sync.WaitGroup, metric string, metricsFolder string) {
	defer wg.Done()

	queryUrl, err := getRangeQueryUrl(httpClient.host, metric)
	if err != nil {
		log.Printf("Error fetching the metric query: %v\n", err)
		return
	}
	response, err := httpClient.Get(queryUrl)
	if err != nil {
		log.Printf("Error fetching the metric: %v\n", err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		log.Printf("Metric fetch not OK: HTTP status code %v\n", response.Status)
		return
	}

	bodyData, err := io.ReadAll(response.Body)
	if err != nil {
		log.Printf("Error reading the metric query response: %v\n", err)
		return
	}
	if err = os.WriteFile(metricsFolder+"/"+metric+".json", bodyData, 0600); err != nil {
		log.Printf("Error writing the metric file: %v\n", err)
		return
	}
}

func getRangeQueryUrl(host string, metric string) (queryUrl string, err error) {
	hostBaseUrl := "https://" + host
	apiUrl, err := url.Parse(hostBaseUrl)
	if err != nil {
		return queryUrl, err
	}
	apiPath, err := url.Parse("api/v1/query_range")
	if err != nil {
		return queryUrl, err
	}
	apiUrl = apiUrl.ResolveReference(apiPath)
	query := apiUrl.Query()
	query.Set("query", metric)
	query.Set("start", fmt.Sprintf("%d", time.Now().Add(-time.Hour).Unix()))
	query.Set("end", fmt.Sprintf("%d", time.Now().Unix()))
	query.Set("step", "60s")

	apiUrl.RawQuery = query.Encode()
	return apiUrl.String(), nil
}
