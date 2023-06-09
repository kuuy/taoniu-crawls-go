package repositories

import (
  "crypto/sha1"
  "encoding/hex"
  "encoding/json"
  "errors"
  "fmt"
  "io/ioutil"
  "net"
  "net/http"
  "net/url"
  "time"

  "github.com/PuerkitoBio/goquery"
  "github.com/hibiken/asynq"
  "github.com/nats-io/nats.go"
  "github.com/rs/xid"
  "github.com/tidwall/gjson"
  "gorm.io/datatypes"
  "gorm.io/gorm"

  "taoniu.local/crawls/spiders/common"
  config "taoniu.local/crawls/spiders/config/queue"
  "taoniu.local/crawls/spiders/models"
  "taoniu.local/crawls/spiders/queue/asynq/jobs"
)

type TasksRepository struct {
  Db                *gorm.DB
  Nats              *nats.Conn
  Asynq             *asynq.Client
  Job               *jobs.Tasks
  SourcesRepository *SourcesRepository
}

func (r *TasksRepository) Source() *SourcesRepository {
  if r.SourcesRepository == nil {
    r.SourcesRepository = &SourcesRepository{
      Db: r.Db,
    }
  }
  return r.SourcesRepository
}

func (r *TasksRepository) Scan(status int) []string {
  var ids []string
  r.Db.Model(&models.Task{}).Where("status", 3).Pluck("id", &ids)
  return ids
}

func (r *TasksRepository) Find(id string) (*models.Task, error) {
  var entity *models.Task
  result := r.Db.First(&entity, "id", id)
  if errors.Is(result.Error, gorm.ErrRecordNotFound) {
    return nil, result.Error
  }
  return entity, nil
}

func (r *TasksRepository) Get(id string) (*models.Task, error) {
  var entity *models.Task
  result := r.Db.Where("id = ?", id).First(&entity)
  if errors.Is(result.Error, gorm.ErrRecordNotFound) {
    return nil, result.Error
  }
  return entity, nil
}

func (r *TasksRepository) GetBySourceID(sourceID string) (*models.Task, error) {
  var entity *models.Task
  result := r.Db.Where("source_id", sourceID).Take(&entity)
  if errors.Is(result.Error, gorm.ErrRecordNotFound) {
    return nil, result.Error
  }
  return entity, nil
}

func (r *TasksRepository) Save(
  parentId string,
  sourceId string,
  url string,
) error {
  hash := sha1.Sum([]byte(url))
  urlSha1 := hex.EncodeToString(hash[:])

  var entity *models.Task
  result := r.Db.Where("url_sha1 = ? AND url = ?", urlSha1, url).Take(&entity)
  if errors.Is(result.Error, gorm.ErrRecordNotFound) {
    entity = &models.Task{
      ID:            xid.New().String(),
      ParentID:      parentId,
      SourceID:      sourceId,
      Url:           url,
      UrlSha1:       urlSha1,
      ExtractResult: map[string]interface{}{},
    }
    r.Db.Create(&entity)
  } else {
    entity.ParentID = parentId
    entity.SourceID = sourceId
    entity.Status = 0
    r.Db.Model(&models.Task{ID: entity.ID}).Updates(entity)
  }

  job, err := r.Job.Process(entity.ID)
  if err == nil {
    r.Asynq.Enqueue(
      job,
      asynq.Queue(config.TASKS),
      asynq.MaxRetry(0),
      asynq.Timeout(5*time.Minute),
    )
  }

  return nil
}

func (r *TasksRepository) Process(task *models.Task) error {
  tr := &http.Transport{
    DisableKeepAlives: true,
  }

  source, err := r.Source().Get(task.SourceID)
  if err != nil {
    return err
  }

  if source.UseProxy {
    session := &common.ProxySession{
      Proxy: fmt.Sprintf("socks5://127.0.0.1:1088?timeout=%ds", source.Timeout),
    }
    tr.DialContext = session.DialContext
  } else {
    session := &net.Dialer{}
    tr.DialContext = session.DialContext
  }

  httpClient := &http.Client{
    Transport: tr,
    Timeout:   time.Duration(source.Timeout) * time.Second,
  }

  req, _ := http.NewRequest("GET", task.Url, nil)
  for key, val := range source.Headers {
    req.Header.Set(key, val.(string))
  }
  resp, err := httpClient.Do(req)
  if err != nil {
    r.Db.Model(&task).Update("status", 3)
    return err
  }
  defer resp.Body.Close()

  if resp.StatusCode != http.StatusOK {
    return errors.New(
      fmt.Sprintf(
        "request error: status[%s] code[%d]",
        resp.Status,
        resp.StatusCode,
      ),
    )
  }

  var content string
  var doc *goquery.Document

  result := make(map[string]interface{})
  for key, value := range source.ExtractRules {
    rules := r.Source().ToExtractRules(value)
    if rules.Html != nil {
      if doc == nil {
        doc, err = goquery.NewDocumentFromReader(resp.Body)
        if err != nil {
          return err
        }
      }
      if rules.Html.List != nil {
        result[key], err = r.Source().ExtractHtmlList(doc, rules.Html)
      } else {
        result[key], err = r.Source().ExtractHtml(doc, rules.Html)
      }
    }
    if rules.Json != nil {
      if _, ok := result[key]; ok {
        content = result[key].(string)
      } else {
        if content == "" {
          body, _ := ioutil.ReadAll(resp.Body)
          content = string(body)
          if content == "" {
            return errors.New("content is empty")
          }
        }
      }
      if rules.Json.List != "" {
        result[key], err = r.Source().ExtractJsonList(content, rules.Json)
        if err != nil {
          continue
        }
      } else {
        result[key], err = r.Source().ExtractJson(content, rules.Json)
        if err != nil {
          continue
        }
      }
    }
  }

  if scroll, ok := source.Params["scroll"]; ok {
    content, err := json.Marshal(result)
    if err == nil {
      items := gjson.GetBytes(content, scroll.(string))
      if items.Exists() {
        items := items.Array()
        score := items[len(items)-1]
        url, err := url.Parse(task.Url)
        if err == nil {
          values := url.Query()
          if items, ok := source.Params["query"].([]interface{}); ok {
            for _, item := range items {
              item := item.(map[string]interface{})
              name := item["name"].(string)
              value := item["value"].(string)
              if value == "$0" {
                continue
              }
              if value == "$1" {
                value = fmt.Sprintf("%v", score)
              }
              values[name] = []string{value}
            }
          }
          url.RawQuery = values.Encode()
          r.Save("", source.ID, url.String())
        }
      }
    }
  }
  task.Status = 1
  task.ExtractResult = r.JSONMap(result)

  r.Db.Model(&models.Task{ID: task.ID}).Updates(task)

  r.Nats.Publish(source.Slug, []byte(task.ID))
  r.Nats.Flush()

  return nil
}

func (r *TasksRepository) JSONMap(in interface{}) datatypes.JSONMap {
  buf, _ := json.Marshal(in)

  var out datatypes.JSONMap
  json.Unmarshal(buf, &out)
  return out
}
