package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config задаёт основные параметры (их можно заменить на флаги/конфигурацию)
type Config struct {
	// Общие настройки
	SkipAutoISPsGCS                          bool
	SkipAutoTLS12BreakageTest                bool
	OutputMostSuccessfulStrategiesSeparately bool

	// Параметры для HTTP‑тестов
	CurlMinTimeout time.Duration

	// Папки и файлы
	MostSuccessfulStrategiesFile string
	CheckListFolder              string
	StrategiesFolder             string
	LogsFolder                   string

	// Стратегия
	FakeSNI     string
	FakeHexRaw  string
	PayloadTLS  string
	PayloadQuic string

	// Тестовые URL
	NetConnTestURL string
	TLS12TestURL   string

	// Report mapping URL (для извлечения cluster codename)
	ReportMappingURLs []string

	// Программы для тестирования
	GdpiName      string
	GdpiExeName   string
	ZapretName    string
	ZapretExeName string
	CiaName       string
	CiaExeName    string

	Version string
}

var config = Config{
	SkipAutoISPsGCS:                          false,
	SkipAutoTLS12BreakageTest:                true,
	OutputMostSuccessfulStrategiesSeparately: false,
	CurlMinTimeout:                           2 * time.Second,
	MostSuccessfulStrategiesFile:             "MostSuccessfulStrategies.txt",
	CheckListFolder:                          "CheckLists",
	StrategiesFolder:                         "Strategies",
	LogsFolder:                               "Logs",
	FakeSNI:                                  "www.google.com",
	FakeHexRaw:                               "1603030135010001310303424143facf5c983ac8ff20b819cfd634cbf5143c0005b2b8b142a6cd335012c220008969b6b387683dedb4114d466ca90be3212b2bde0c4f56261a9801",
	PayloadTLS:                               "tls_earth_google_com.bin",
	PayloadQuic:                              "quic_ietf_www_google_com.bin",
	NetConnTestURL:                           "https://ya.ru",
	TLS12TestURL:                             "https://tls-v1-2.badssl.com:1012",
	ReportMappingURLs: []string{
		"https://redirector.gvt1.com/report_mapping?di=no",
		"https://redirector.googlevideo.com/report_mapping?di=no",
	},
	GdpiName:      "GoodbyeDPI",
	GdpiExeName:   "goodbyedpi.exe",
	ZapretName:    "Zapret",
	ZapretExeName: `C:\zapret-discord-youtube-1.6.2\bin\winws.exe`,
	CiaName:       "ByeDPI",
	CiaExeName:    "ciadpi.exe",
	Version:       "v1.3.01",
}

func main() {
	// Настройка логирования (лог пишется и в консоль, и в файл)
	logFile := setupLogFile()
	defer logFile.Close()

	log.Println("Starting GoodCheck", config.Version)

	// Флаги командной строки
	provider := flag.String("provider", "", "Тестовая программа: gdpi, zapret или cia")
	passes := flag.Int("passes", 1, "Количество проходов тестирования")
	strategyFile := flag.String("strategy", "", "Файл со списком стратегий")
	checklistFile := flag.String("checklist", "", "Файл с дополнительными URL для проверки (необязательно)")
	flag.Parse()

	// Если не указан провайдер, запрашиваем выбор
	if *provider == "" {
		fmt.Println("Выберите тестовую программу:")
		choices := []string{}
		if fileExists(config.GdpiExeName) {
			choices = append(choices, "gdpi")
			fmt.Println("1:", config.GdpiName)
		}
		if fileExists(config.ZapretExeName) {
			choices = append(choices, "zapret")
			fmt.Println("2:", config.ZapretName)
		}
		if fileExists(config.CiaExeName) {
			choices = append(choices, "cia")
			fmt.Println("3:", config.CiaName)
		}
		fmt.Println("0: Отмена")
		var choice int
		fmt.Print("Введите номер: ")
		fmt.Scanln(&choice)
		if choice <= 0 || choice > len(choices) {
			log.Println("Отмена")
			return
		}
		*provider = choices[choice-1]
	}

	// Если не указан файл стратегий, спрашиваем у пользователя
	if *strategyFile == "" {
		fmt.Printf("Введите имя файла со стратегиями (в папке %s): ", config.StrategiesFolder)
		var input string
		fmt.Scanln(&input)
		*strategyFile = filepath.Join(config.StrategiesFolder, input)
	}
	strategies, err := readLines(*strategyFile)
	if err != nil {
		log.Println("Ошибка чтения файла стратегий:", err)
		return
	}
	log.Println("Загружено стратегий:", len(strategies))

	// Составляем список тестовых URL
	var testURLs []string
	if !config.SkipAutoTLS12BreakageTest {
		testURLs = append(testURLs, config.TLS12TestURL)
	} else {
		log.Println("Пропуск TLS 1.2 теста")
	}

	if !config.SkipAutoISPsGCS {
		clusterCodename, err := fetchClusterCodename(config.ReportMappingURLs)
		if err != nil {
			log.Println("Предупреждение: не удалось получить cluster codename:", err)
		} else {
			log.Println("Cluster codename:", clusterCodename)
			autoGCS := computeAutoGCS(clusterCodename)
			testURLs = append(testURLs, autoGCS)
			log.Println("Используется автоадрес ISP's GCS:", autoGCS)
		}
	} else {
		log.Println("Пропуск теста ISP's GCS")
	}

	// Если указан файл с чеклистом, добавляем его URL
	if *checklistFile != "" && fileExists(*checklistFile) {
		lines, err := readLines(*checklistFile)
		if err != nil {
			log.Println("Ошибка чтения чеклиста:", err)
		} else {
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line != "" {
					if !strings.HasPrefix(line, "http://") && !strings.HasPrefix(line, "https://") {
						line = "https://" + line
					}
					testURLs = append(testURLs, line)
				}
			}
		}
	}

	if len(testURLs) == 0 {
		log.Println("Нет URL для проверки, завершаем работу.")
		return
	}
	log.Println("Всего URL для проверки:", len(testURLs))

	if *passes < 1 {
		*passes = 1
	}
	log.Println("Количество проходов:", *passes)

	// Для каждой стратегии запускаем тестирование
	results := make(map[string]int)
	for _, strat := range strategies {
		log.Println("Тестирование стратегии:", strat)
		successes := runStrategyTest(*provider, strat, *passes, testURLs, config.CurlMinTimeout)
		results[strat] = successes
		log.Printf("Стратегия: %s, успехов: %d из %d\n", strat, successes, len(testURLs))
	}

	// Выводим итоговый отчёт
	log.Println("Тестирование завершено. Результаты:")
	for strat, succ := range results {
		fmt.Printf("Стратегия: %s -> %d/%d успешных запросов\n", strat, succ, len(testURLs))
	}

	// Если нужно – записываем самые успешные стратегии в отдельный файл
	if config.OutputMostSuccessfulStrategiesSeparately {
		mostSuccess := -1
		for _, s := range results {
			if s > mostSuccess {
				mostSuccess = s
			}
		}
		file, err := os.Create(config.MostSuccessfulStrategiesFile)
		if err != nil {
			log.Println("Ошибка создания файла для лучших стратегий:", err)
		} else {
			defer file.Close()
			for strat, s := range results {
				if s == mostSuccess {
					file.WriteString(strat + "\n")
				}
			}
			log.Println("Лучшие стратегии записаны в", config.MostSuccessfulStrategiesFile)
		}
	}

	log.Println("Лог сохранён. Завершение работы.")
}

// runStrategyTest запускает внешний процесс (провайдера) с аргументом стратегии,
// ждёт указанное число проходов, проводит параллельные HTTP‑запросы и возвращает минимальное число успешных ответов.
func runStrategyTest(provider, strategy string, passes int, testURLs []string, timeout time.Duration) int {
	var exeName string
	switch provider {
	case "gdpi":
		exeName = config.GdpiExeName
	case "zapret":
		exeName = config.ZapretExeName
	case "cia":
		exeName = config.CiaExeName
	default:
		log.Println("Неизвестный провайдер:", provider)
		return 0
	}

	// Запускаем внешний процесс (ожидается, что файл с именем exeName находится в рабочей папке)
	cmd := exec.Command(exeName, strategy)
	if err := cmd.Start(); err != nil {
		log.Println("Ошибка запуска процесса", exeName, ":", err)
		return 0
	}
	// Немного ждём для инициализации процесса
	time.Sleep(1 * time.Second)

	lowestSuccess := int(^uint(0) >> 1) // максимально большое число
	for i := 0; i < passes; i++ {
		succ := runHTTPTests(testURLs, timeout)
		if succ < lowestSuccess {
			lowestSuccess = succ
		}
		log.Printf("Проход %d: %d/%d успешных запросов\n", i+1, succ, len(testURLs))
	}
	// Завершаем процесс
	if err := cmd.Process.Kill(); err != nil {
		log.Println("Ошибка завершения процесса", exeName, ":", err)
	}
	return lowestSuccess
}

// runHTTPTests проводит параллельные HTTP‑запросы по списку URL и возвращает число успешных ответов.
func runHTTPTests(urls []string, timeout time.Duration) int {
	var wg sync.WaitGroup
	var successes int32 = 0
	client := &http.Client{
		Timeout: timeout,
	}
	for _, url := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			resp, err := client.Get(u)
			if err == nil {
				if resp.StatusCode >= 200 && resp.StatusCode != 403 && resp.StatusCode < 405 {
					atomic.AddInt32(&successes, 1)
					log.Println("Работает:", u, "->", resp.Status)
				} else {
					log.Println("Не работает:", u, "->", resp.Status)
				}
				resp.Body.Close()
			} else {
				log.Println("Ошибка при запросе", u, ":", err)
			}
		}(url)
	}
	wg.Wait()
	return int(successes)
}

// fetchClusterCodename пытается получить cluster codename по очереди с report‑mapping URL.
func fetchClusterCodename(urls []string) (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	for _, url := range urls {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		// Разбиваем по пробелам и берем третий токен
		tokens := strings.Fields(string(body))
		if len(tokens) >= 3 {
			return tokens[2], nil
		}
	}
	return "", fmt.Errorf("не удалось получить cluster codename")
}

// computeAutoGCS преобразует cluster codename в URL ISP‑Google Cache Server.
// Используется преобразование символов по двум спискам (аналогично batch‑скрипту).
func computeAutoGCS(codename string) string {
	lettersListA := strings.Split("u z p k f a 5 0 v q l g b 6 1 w r m h c 7 2 x s n i d 8 3 y t o j e 9 4 -", " ")
	lettersListB := strings.Split("0 1 2 3 4 5 6 7 8 9 a b c d e f g h i j k l m n o p q r s t u v w x y z -", " ")

	var clusterName string
	for _, ch := range codename {
		letter := string(ch)
		for i, a := range lettersListA {
			if a == letter {
				clusterName += lettersListB[i]
				break
			}
		}
	}
	// Формируем URL, аналогично оригинальному скрипту
	return "https://rr1---sn-" + clusterName + ".googlevideo.com"
}

// readLines читает все непустые строки из указанного файла.
func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// fileExists проверяет, существует ли файл с данным именем.
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

// setupLogFile создаёт папку для логов (если её нет) и файл лога с таймстампом.
func setupLogFile() *os.File {
	if _, err := os.Stat(config.LogsFolder); os.IsNotExist(err) {
		os.Mkdir(config.LogsFolder, 0755)
	}
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	logFileName := filepath.Join(config.LogsFolder, "availability_check_"+timestamp+".txt")
	file, err := os.Create(logFileName)
	if err != nil {
		log.Println("Не удалось создать лог-файл:", err)
		os.Exit(1)
	}
	// Лог одновременно пишется в консоль и в файл
	log.SetOutput(io.MultiWriter(os.Stdout, file))
	return file
}
