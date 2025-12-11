// @author:llychao<lychao_vip@163.com>
// @contributor: Junyi<me@junyi.pw>
// @date:2020-02-18
// @功能:golang m3u8 video Downloader
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	
)

const (
	// HEAD_TIMEOUT 请求头超时时间
	HEAD_TIMEOUT = 30 * time.Second
	// PROGRESS_WIDTH 进度条长度
	PROGRESS_WIDTH = 20
	// TS_NAME_TEMPLATE ts视频片段命名规则
	TS_NAME_TEMPLATE = "%05d.ts"
)

var (
	// 命令行参数
	urlFlag = flag.String("u", "", "m3u8下载地址(http(s)://url/xx/xx/index.m3u8)")
	nFlag   = flag.Int("n", 24, "num:下载线程数(默认24)")
	htFlag  = flag.String("ht", "v1", "hostType:设置getHost的方式(v1: `http(s):// + url.Host + filepath.Dir(url.Path)`; v2: `http(s)://+ u.Host`")
	oFlag   = flag.String("o", "movie", "movieName:自定义文件名(默认为movie)不带后缀")
	cFlag   = flag.String("c", "", "cookie:自定义请求cookie")
	rFlag   = flag.Bool("r", true, "autoClear:是否自动清除ts文件")
	sFlag   = flag.Int("s", 0, "InsecureSkipVerify:是否允许不安全的请求(默认0)")
	spFlag  = flag.String("sp", "", "savePath:文件保存的绝对路径(默认为当前路径,建议默认值)")
	clFlag  = flag.Bool("checklen", true, "开启媒体文件 大小 检查(默认开启)")

	logger *log.Logger

	defaultHeaders = map[string]string{
		"User-Agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/79.0.3945.88 Safari/537.36",
		"Connection":      "keep-alive",
		"Accept":          "*/*",
		"Accept-Encoding": "*", // 注意：服务端可能压缩响应，需要正确处理
		"Accept-Language": "zh-CN,zh;q=0.9, en;q=0.8, de;q=0.7, *;q=0.5",
	}
)


type TsInfo struct {
	Name string
	Url  string
}

func init() {
	logger = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)
}

func main() {
	Run()
}

func Run() {
	msgTpl := "[功能]:多线程下载直播流m3u8视屏\n[提醒]:下载失败，请使用 -ht=v2 \n[提醒]:下载失败，m3u8 地址可能存在嵌套\n[提醒]:进度条中途下载失败，可重复执行"
	fmt.Println(msgTpl)
	runtime.GOMAXPROCS(runtime.NumCPU())
	now := time.Now()

	// 1、解析命令行参数
	flag.Parse()
	m3u8Url := *urlFlag
	maxGoroutines := *nFlag
	hostType := *htFlag
	movieName := *oFlag
	autoClearFlag := *rFlag
	cookie := *cFlag
	savePath := *spFlag
	checkLen := *clFlag

	defaultHeaders["Referer"] = getHost(m3u8Url, "v2")
	if cookie != "" {
		defaultHeaders["Cookie"] = cookie
	}

	
	if !strings.HasPrefix(m3u8Url, "http") || m3u8Url == "" {
		flag.Usage()
		return
	}
	var download_dir string
	pwd, _ := os.Getwd()
	if savePath != "" {
		pwd = savePath
	}
	// 初始化下载ts的目录，后面所有的ts文件会保存在这里
	download_dir = filepath.Join(pwd, movieName)
	if isExist, _ := pathExists(download_dir); !isExist {
		os.MkdirAll(download_dir, os.ModePerm)
	}

	// 2、解析m3u8
	m3u8Host := getHost(m3u8Url, hostType)
	m3u8Body := getM3u8Body(m3u8Url)
	//m3u8Body := getFromFile()
	ts_key := getM3u8Key(m3u8Host, m3u8Body)
	if ts_key != "" {
		fmt.Printf("待解密 ts 文件 key : %s \n", ts_key)
	}
	ts_list := getTsList(m3u8Host, m3u8Body)
	fmt.Println("待下载 ts 文件数量:", len(ts_list))

	// 3、下载ts文件到download_dir
	downloader(ts_list, maxGoroutines, download_dir, ts_key, checkLen)
	if ok := checkTsDownDir(download_dir); !ok {
		fmt.Printf("\n[Failed] 请检查url地址有效性 \n")
		return
	}

	// 4、合并ts切割文件成mp4文件
	mv := mergeTs(download_dir)
	if autoClearFlag {
		//自动清除ts文件目录
		os.RemoveAll(download_dir)
	}

	//5、输出下载视频信息
	fmt.Printf("\n[Success] 下载保存路径：%s | 共耗时: %6.2fs\n", mv, time.Now().Sub(now).Seconds())
}

func requestGet(url string) (*http.Response, error) {

	insecure := *sFlag
	
	// 创建带超时的请求
	req, err := http.NewRequestWithContext(
		context.Background(), // 或者传入一个带有取消信号的 context
		http.MethodGet,
		url,
		nil, // GET 请求通常没有 body
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// 设置 Headers
	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}


	insecureSkipVerify := false
	if insecure != 0 {
		insecureSkipVerify = true
	}
	
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecureSkipVerify, // 设置跳过证书验证
		},
	}
	
	// 创建 HTTP 客户端
	client := &http.Client{
		Transport: transport,
		Timeout:   HEAD_TIMEOUT,
	}

	// 执行请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}


// 获取m3u8地址的host
func getHost(Url, ht string) (host string) {
	u, err := url.Parse(Url)
	checkErr(err)
	switch ht {
	case "v1":
		host = u.Scheme + "://" + u.Host + path.Dir(u.EscapedPath())
	case "v2":
		host = u.Scheme + "://" + u.Host
	}
	return
}

// 获取m3u8地址的内容体
func getM3u8Body(Url string) string {

	resp := requestGet(Url)
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	checkErr(err)
	
	return string(data)
}

// 获取m3u8加密的密钥
func getM3u8Key(host, html string) (key string) {
	lines := strings.Split(html, "\n")
	key = ""
	for _, line := range lines {

		line = strings.TrimSpace(line)
		
		if strings.Contains(line, "#EXT-X-KEY") {
			if !strings.Contains(line, "URI") {
				continue
			}
			fmt.Println("[debug] line_key:",line)
			uri_pos := strings.Index(line, "URI")
			quotation_mark_pos := strings.LastIndex(line, "\"")
			key_url := strings.Split(line[uri_pos:quotation_mark_pos], "\"")[1]
			if !strings.Contains(line, "http") {
				key_url = fmt.Sprintf("%s/%s", host, key_url)
			}



			resp := requestGet(key_url)
			defer resp.Body.Close()
			
			if resp.StatusCode == 200 {
				data, err := io.ReadAll(resp.Body)
				checkErr(err)
				key = string(data)
				break
			}

			
		}
	}
	fmt.Println("[debug] m3u8Host:",host,"m3u8Key:",key)
	return
}

func getTsList(host, body string) (tsList []TsInfo) {
	lines := strings.Split(body, "\n")
	index := 0
	var ts TsInfo
	for _, line := range lines {
		
		if !strings.HasPrefix(line, "#") && line != "" {
			//有可能出现的二级嵌套格式的m3u8,请自行转换！
			index++
			
			line = strings.TrimSpace(line)
			
			if strings.HasPrefix(line, "http") {
				ts = TsInfo{
					Name: fmt.Sprintf(TS_NAME_TEMPLATE, index),
					Url:  line,
				}
				tsList = append(tsList, ts)
			} else {
				line = strings.TrimPrefix(line, "/")
				ts = TsInfo{
					Name: fmt.Sprintf(TS_NAME_TEMPLATE, index),
					Url:  fmt.Sprintf("%s/%s", host, line),
				}
				tsList = append(tsList, ts)
			}
		}
	}
	return
}

func getFromFile() string {
	data, _ := ioutil.ReadFile("./ts.txt")
	return string(data)
}


func downloadTsFile(ts TsInfo, download_dir, key string, retries int, checkLen bool) {
	
	if retries <= 0 {
		fmt.Printf("[ERROR] Max retries exceeded for: %s\n", ts.Url)
		return
	}

	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[WARN] Panic during download (retrying): %v for %s\n", r, ts.Url)
			downloadTsFile(ts, download_dir, key, retries-1, checkLen)
		}
	}()

	curr_path_file := filepath.Join(download_dir, ts.Name)
	if isExist, _ := pathExists(curr_path_file); isExist {
		fmt.Printf("[WARN] File already exists, skipping: %s\n", curr_path_file)
		return
	}

	resp := requestGet(ts.Url)
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Printf("[ERROR] Request failed for %s: status code %d\n", ts.Url, resp.StatusCode)
		if retries > 0 {
			fmt.Printf("[INFO] Retrying... (%d left)\n", retries-1)
			time.Sleep(time.Second)
			downloadTsFile(ts, download_dir, key, retries-1, checkLen)
		}
		return
	}

	// 读取响应体
	origData, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("[ERROR] Failed to read response body for %s: %v\n", ts.Url, err)
		if retries > 0 {
			fmt.Printf("[INFO] Retrying... (%d left)\n", retries-1)
			time.Sleep(time.Second)
			downloadTsFile(ts, download_dir, key, retries-1, checkLen)
		}
		return
	}

	contentLen := 0
	contentLenStr := resp.Header.Get("Content-Length")
	if contentLenStr != "" {
		contentLen, _ = strconv.Atoi(contentLenStr)
	}

	// 校验长度是否合法
	if checkLen {
		if len(origData) == 0 || (contentLen > 0 && len(origData) < contentLen) {
			fmt.Printf("\n")
			fmt.Printf("[WARN] Invalid response data for %s: len=%d, content-length=%d\n",
				ts.Url, len(origData), contentLen)
			if retries > 0 {
				time.Sleep(time.Second)
				downloadTsFile(ts, download_dir, key, retries-1, checkLen)
			}
			return
		}
	}

	// 解密出视频 ts 源文件
	if key != "" {
		fmt.Printf("[DEBUG] Decrypting segment: %s\n", ts.Name)
		var err error
		origData, err = AesDecrypt(origData, []byte(key))
		if err != nil {
			fmt.Printf("[ERROR] Decryption failed for %s: %v\n", ts.Name, err)
			if retries > 0 {
				time.Sleep(time.Second)
				downloadTsFile(ts, download_dir, key, retries-1, checkLen)
			}
			return
		}
	}

	// SyncByte 修复
	syncByte := uint8(71) // 0x47
	bLen := len(origData)
	origStart := 0
	for j := 0; j < bLen; j++ {
		if origData[j] == syncByte {
			origStart = j
			break
		}
	}
	if origStart > 0 {
		fmt.Printf("[DEBUG] Trimmed %d bytes before SyncByte in %s\n", origStart, ts.Name)
	}
	origData = origData[origStart:]

	err = ioutil.WriteFile(curr_path_file, origData, 0666)
	if err != nil {
		fmt.Printf("[ERROR] Failed to write file %s: %v\n", curr_path_file, err)
	} else {
		// fmt.Printf("\tSaved %s (%d bytes)       \n", ts.Name, len(origData))
	}
	
}

// downloader m3u8 下载器
func downloader(tsList []TsInfo, maxGoroutines int, downloadDir string, key string, checkLen bool) {
	retry := 500 //单个ts 下载重试次数
	var wg sync.WaitGroup
	limiter := make(chan struct{}, maxGoroutines) //chan struct 内存占用 0 bool 占用 1
	tsLen := len(tsList)

	fmt.Println("[downloader]待下载 ts 文件数量:", tsLen)
	
	downloadCount := 0
	for _, ts := range tsList {
		wg.Add(1)
		limiter <- struct{}{}
		go func(ts TsInfo, downloadDir, key string, retryies int) {
			defer func() {
				wg.Done()
				<-limiter
			}()
			downloadTsFile(ts, downloadDir, key, retryies, checkLen)
			downloadCount++
			DrawProgressBar("Downloading", float32(downloadCount), float32(tsLen), PROGRESS_WIDTH, ts.Name)
			return
		}(ts, downloadDir, key, retry)
	}
	wg.Wait()
}

func checkTsDownDir(dir string) bool {
	if isExist, _ := pathExists(filepath.Join(dir, fmt.Sprintf(TS_NAME_TEMPLATE, 0))); !isExist {
		return true
	}
	return false
}

// 合并ts文件
func mergeTs(downloadDir string) string {
	mvName := downloadDir + ".mp4"
	outMv, _ := os.Create(mvName)
	defer outMv.Close()
	writer := bufio.NewWriter(outMv)
	err := filepath.Walk(downloadDir, func(path string, f os.FileInfo, err error) error {
		if f == nil {
			return err
		}
		if f.IsDir() || filepath.Ext(path) != ".ts" {
			return nil
		}
		bytes, _ := ioutil.ReadFile(path)
		_, err = writer.Write(bytes)

		fmt.Print("\r\033[K Merging... " + path)
		
		return err
	})
	checkErr(err)
	_ = writer.Flush()


	fmt.Print("\n")
	return mvName
}

// 进度条
func DrawProgressBar(prefix string, proportion float32, total float32, width int, suffix ...string) {

	percent := proportion / total
	
	pos := int(percent * float32(width))
	s := fmt.Sprintf("[%s] %s%*s %6.2f%%  %d/%d    %-15s ",
		prefix, strings.Repeat("■", pos), width-pos, "", percent*100, int(proportion), int(total), strings.Join(suffix, ""))
	fmt.Print("\r\033[K" + s)
}

// ============================== shell相关 ==============================
// 判断文件是否存在
func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// 执行 shell
func execUnixShell(s string) {
	cmd := exec.Command("bash", "-c", s)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s", out.String())
}

func execWinShell(s string) error {
	cmd := exec.Command("cmd", "/C", s)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return err
	}
	fmt.Printf("%s", out.String())
	return nil
}

// windows 合并文件
func win_merge_file(path string) {
	pwd, _ := os.Getwd()
	os.Chdir(path)
	execWinShell("copy /b *.ts merge.tmp")
	execWinShell("del /Q *.ts")
	os.Rename("merge.tmp", "merge.mp4")
	os.Chdir(pwd)
}

// unix 合并文件
func unix_merge_file(path string) {
	pwd, _ := os.Getwd()
	os.Chdir(path)
	//cmd := `ls  *.ts |sort -t "\." -k 1 -n |awk '{print $0}' |xargs -n 1 -I {} bash -c "cat {} >> new.tmp"`
	cmd := `cat *.ts >> merge.tmp`
	execUnixShell(cmd)
	execUnixShell("rm -rf *.ts")
	os.Rename("merge.tmp", "merge.mp4")
	os.Chdir(pwd)
}

// ============================== 加解密相关 ==============================

func PKCS7Padding(ciphertext []byte, blockSize int) []byte {
	padding := blockSize - len(ciphertext)%blockSize
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(ciphertext, padtext...)
}

func PKCS7UnPadding(origData []byte) []byte {
	length := len(origData)
	unpadding := int(origData[length-1])
	return origData[:(length - unpadding)]
}

func AesEncrypt(origData, key []byte, ivs ...[]byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	blockSize := block.BlockSize()
	var iv []byte
	if len(ivs) == 0 {
		iv = key
	} else {
		iv = ivs[0]
	}
	origData = PKCS7Padding(origData, blockSize)
	blockMode := cipher.NewCBCEncrypter(block, iv[:blockSize])
	crypted := make([]byte, len(origData))
	blockMode.CryptBlocks(crypted, origData)
	return crypted, nil
}

func AesDecrypt(crypted, key []byte, ivs ...[]byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	blockSize := block.BlockSize()
	var iv []byte
	if len(ivs) == 0 {
		iv = key
	} else {
		iv = ivs[0]
	}
	blockMode := cipher.NewCBCDecrypter(block, iv[:blockSize])
	origData := make([]byte, len(crypted))
	blockMode.CryptBlocks(origData, crypted)
	origData = PKCS7UnPadding(origData)
	return origData, nil
}

func checkErr(e error) {
	if e != nil {
		logger.Panic(e)
	}
}
