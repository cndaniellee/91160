package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/robfig/cron/v3"
	"github.com/spf13/viper"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Config struct {
	UserId     string `yaml:"userId"`
	UnitId     string `yaml:"unitId"`
	DepId      string `yaml:"depId"`
	MemberId   string `yaml:"memberId"`
	JSessionId string `yaml:"jSessionId"`
}

type Doctor struct {
	DoctorName string
	DoctorId   int
	DepId      int
	Zcid       string
}

type Schedule struct {
	DoctorName string
	DoctorId   int
	DepId      int
	Zcid       string
	ToDate     string
	ScheduleId string
	BeginTime  string
	EndTime    string
	DetlId     string
}

var (
	config             = &Config{}
	availableDoctors   []*Doctor
	availableSchedules []*Schedule
	refreshMutex       sync.Mutex
	exitChan           = make(chan struct{}, 1)
)

func main() {
	yaml := "./config.yaml"
	if arg := flag.Arg(0); arg != "" {
		yaml = arg
	}

	// 加载配置文件
	viper.SetConfigFile(yaml)
	if err := viper.ReadInConfig(); err != nil {
		log.Fatal(fmt.Errorf("Config read failed: %s \n", err))
	}
	if err := viper.Unmarshal(config); err != nil {
		log.Fatal(fmt.Errorf("Config unmarshal failed: %s \n", err))
	}
	viper.WatchConfig()

	if res := checkMember(); res != "true" {
		log.Fatal("会员信息检查失败，请在任意提交页尝试提交并补全信息")
	}

	// 先刷新一次医生列表
	refreshDoctor()

	// 设置定时任务
	c := cron.New()
	// 每5分钟刷新医生列表
	_, _ = c.AddFunc("@every 5m", refreshDoctor)
	// 每5秒更新排班
	_, _ = c.AddFunc("@every 5s", refreshSchedule)
	// 每秒抢票
	_, _ = c.AddFunc("@every 1s", executeReserve)
	c.Start()
	select {
	case <-exitChan:
		break
	}
}

/*
定时任务
*/

// 刷新医生列表
func refreshDoctor() {
	var doctors []*Doctor
	if res := fetchDoctors(); res != nil {
		log.Printf(">>> 获取到%v名医师信息", len(res.Data.Rows))
		for _, item := range res.Data.Rows {
			if item.Zcid == "主任医师" || item.Zcid == "副主任医师" {
				log.Printf("--选取医师：%v（%v）", strings.Split(item.DoctorName, "-")[0], item.Zcid)
				doctors = append(doctors, &Doctor{
					DoctorName: item.DoctorName,
					DoctorId:   item.DoctorId,
					DepId:      item.DepId,
					Zcid:       item.Zcid,
				})
			}
		}
	}
	if len(doctors) == 0 {
		log.Println(">>> 未获取到医师信息")
	}
	refreshMutex.Lock()
	availableDoctors = doctors
	refreshMutex.Unlock()
	refreshSchedule()
}

// 刷新排班列表
func refreshSchedule() {
	var schedules []*Schedule
	for _, doctor := range availableDoctors {
		if res := fetchSchedule(doctor.DoctorId); res != nil {
			for _, schedule := range res.Data.Sch {
				if schedule.YState == "1" {
					log.Printf("%v（%v）排班：%v %v（余%v个）", strings.Split(doctor.DoctorName, "-")[0], doctor.Zcid, schedule.ToDate, schedule.TimeTypeDesc, schedule.LeftNum)
					if periodRes := fetchPeriods(doctor.DoctorId, schedule.TimeType, schedule.ScheduleId); res != nil {
						for _, period := range periodRes.Data {
							log.Printf("--%v（余%v个）", period.DetlTimeDesc, period.YuyueNum)
							schedules = append(schedules, &Schedule{
								DoctorName: doctor.DoctorName,
								DoctorId:   doctor.DoctorId,
								DepId:      doctor.DepId,
								Zcid:       doctor.Zcid,
								ToDate:     schedule.ToDate,
								ScheduleId: schedule.ScheduleId,
								BeginTime:  period.BeginTime,
								EndTime:    period.EndTime,
								DetlId:     period.DetlId,
							})
						}
					}
				}
			}
		}
		// 睡200毫秒，避免被查水表
		time.Sleep(time.Microsecond * 200)
	}
	if len(schedules) == 0 {
		log.Println(">>> 未获取到可用排班")
	}
	refreshMutex.Lock()
	availableSchedules = schedules
	refreshMutex.Unlock()
}

// 执行预约
func executeReserve() {
	refreshMutex.Lock()
	for i := len(availableSchedules) - 1; i >= 0; i-- {
		item := availableSchedules[i]
		log.Println(">>> 发起预约...")
		if timestamp := submitReserve(item.DoctorId, item.ScheduleId, item.DetlId, item.DoctorName, item.Zcid, item.ToDate, item.BeginTime, item.EndTime); timestamp != "" {
			if orderId := submitConfirm(timestamp); orderId != "" {
				log.Printf(">>> 预约成功！订单编号：%v", orderId)
				exitChan <- struct{}{}
				break
			}
		}
	}
	refreshMutex.Unlock()
}

/*
核心请求
*/

type doctorResponse struct {
	Code string `json:"code"`
	Data struct {
		Rows []struct {
			DoctorName string `json:"doctorName"`
			DoctorId   int    `json:"doctorId"`
			DepId      int    `json:"depId"`
			Zcid       string `json:"zcid"`
		} `json:"rows"`
	} `json:"data"`
}

// 获取医生列表
func fetchDoctors() *doctorResponse {
	httpClient := &http.Client{}
	request, err := http.NewRequest("GET", fmt.Sprintf("https://wxis.91160.com/wxis/doc/getDocListByTime.do?depId=%v&unitId=%v", config.DepId, config.UnitId), nil)
	if err != nil {
		log.Printf("获取医生列表错误: failed new request: %v", err)
		return nil
	}
	response, err := httpClient.Do(request)
	if err != nil {
		log.Printf("获取医生列表错误: failed do request: %v", err)
		return nil
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Printf("获取医生列表错误: failed read response: %v", err)
		return nil
	}
	res := &doctorResponse{}
	err = json.Unmarshal(body, &res)
	if err != nil {
		log.Printf("获取医生列表错误: failed unmarshal json: %v\n%v", err, string(body))
		return nil
	}
	if res.Code != "success" {
		log.Printf("获取医生列表错误：\n%v", string(body))
		return nil
	}
	return res
}

type scheduleResponse struct {
	Status string `json:"status"`
	Data   struct {
		Sch []struct {
			YState       string `json:"y_state"`  // 可预约=1
			LeftNum      string `json:"left_num"` // 余号
			ToDate       string `json:"to_date"`
			TimeType     string `json:"time_type"`
			TimeTypeDesc string `json:"time_type_desc"`
			ScheduleId   string `json:"schedule_id"`
		} `json:"sch"`
	} `json:"data"`
}

// 获取排班列表
func fetchSchedule(doctorId int) *scheduleResponse {
	httpClient := &http.Client{}
	request, err := http.NewRequest("GET", fmt.Sprintf("https://wxis.91160.com/wxis/sch_new/schedulelist.do?unit_id=%v&dep_id=%v&doctor_id=%v&cur_dep_id=%v&unit_name=北京大学深圳医院&dep_name=牙槽外科（拔牙）", config.UnitId, config.DepId, doctorId, config.DepId), nil)
	if err != nil {
		log.Printf("获取排班信息错误: failed new request: %v", err)
		return nil
	}
	// 添加身份Cookie
	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
	response, err := httpClient.Do(request)
	if err != nil {
		log.Printf("获取排班信息错误: failed do request: %v", err)
		return nil
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Printf("获取排班信息错误: failed read response: %v", err)
		return nil
	}
	res := &scheduleResponse{}
	err = json.Unmarshal(body, &res)
	if err != nil {
		log.Printf("获取排班信息错误: failed unmarshal json: %v\n%v", err, string(body))
		return nil
	}
	if res.Status != "1" {
		log.Printf("获取排班信息错误：\n%v", string(body))
		return nil
	}
	return res
}

type periodsResponse struct {
	Status string `json:"status"`
	Data   []struct {
		BeginTime    string `json:"begin_time"`
		EndTime      string `json:"end_time"`
		DetlTimeDesc string `json:"detl_time_desc"`
		YuyueNum     int    `json:"yuyue_num"`
		DetlId       string `json:"detl_id"`
	}
}

// 获取时间列表
func fetchPeriods(doctorId int, timeType, scheduleId string) *periodsResponse {
	httpClient := &http.Client{}
	request, err := http.NewRequest("GET", fmt.Sprintf("https://wxis.91160.com/wxis/sch_new/detlnew.do?unit_detl_map=[{\"unit_id\":\"%v\",\"doctor_id\":\"%v\",\"dep_id\":\"%v\",\"schedule_id\":\"%v\",\"time_type\":\"%v\"}]", config.UnitId, doctorId, config.DepId, scheduleId, timeType), nil)
	if err != nil {
		log.Printf("获取时间信息错误: failed new request: %v", err)
		return nil
	}
	// 添加身份Cookie
	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
	response, err := httpClient.Do(request)
	if err != nil {
		log.Printf("获取时间信息错误: failed do request: %v", err)
		return nil
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Printf("获取时间信息错误: failed read response: %v", err)
		return nil
	}
	res := &periodsResponse{}
	err = json.Unmarshal(body, &res)
	if err != nil {
		log.Printf("获取时间信息错误: failed unmarshal json: %v\n%v", err, string(body))
		return nil
	}
	if res.Status != "1" {
		log.Printf("获取时间信息错误：\n%v", string(body))
		return nil
	}
	return res
}

// 发起预约
func submitReserve(doctorId int, scheduleId, detlId, doctorName, doctorLevel, toDate, beginTime, endTime string) string {
	httpClient := &http.Client{}
	request, err := http.NewRequest("GET", fmt.Sprintf("https://wxis.91160.com/wxis/addOrder/main.do?r=%v&unit_id=%v&branch_id=%v&dep_id=%v&doc_id=%v&sch_id=%v&detl=%v&dep_name=牙槽外科（拔牙）&doc_name=%v&doc_level=%v&amt=33.0&sch_date=%v&riseamt=37.95&begin_time=%v&end_time=%v&origin_unit_id=%v&method=sch1&srcext_type=",
		time.Now().UnixMicro(), config.UnitId, config.UnitId, config.DepId, doctorId, scheduleId, detlId, doctorName, doctorLevel, toDate, beginTime, endTime, config.UnitId), nil)
	if err != nil {
		log.Printf("发起预约失败: failed new request: %v", err)
		return ""
	}
	// 添加身份Cookie
	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
	response, err := httpClient.Do(request)
	if err != nil {
		log.Printf("发起预约失败: failed do request: %v", err)
		return ""
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Printf("发起预约失败: failed read response: %v", err)
		return ""
	}
	reg := regexp.MustCompile(`buildOrder.do\?r=\d+`)
	return strings.Split(reg.FindString(string(body)), "=")[1]
}

// 确认预约
func submitConfirm(timestamp string) string {
	httpClient := &http.Client{}
	request, err := http.NewRequest("POST", fmt.Sprintf("https://wxis.91160.com/wxis//act/order/buildOrder.do?r=%v&method=sch1",
		timestamp), strings.NewReader(fmt.Sprintf("branchId=%v&payway=14&socialType=&mid=%v&yuyueUserType=&memberType=", config.UnitId, config.MemberId)))
	if err != nil {
		log.Printf("提交订单失败: failed new request: %v", err)
		return ""
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// 添加身份Cookie
	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
	response, err := httpClient.Do(request)
	if err != nil {
		log.Printf("提交订单失败: failed do request: %v", err)
		return ""
	}
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Printf("提交订单失败: failed read body: %v", err)
		return ""
	}
	reg := regexp.MustCompile(`order_id: '\d+'`)
	field := reg.FindString(string(body))
	if field == "" {
		log.Printf("提交订单失败: \n%v", string(body))
		return ""
	}
	return strings.Split(field, "'")[1]
}

/*
验证请求
*/

func checkMember() string {
	httpClient := &http.Client{}
	request, err := http.NewRequest("POST", fmt.Sprintf("https://wxis.91160.com/wxis/act/order/checkMember.do?r=%v",
		time.Now().UnixMicro()), strings.NewReader("memberId="+config.MemberId))
	if err != nil {
		log.Printf("检查身份信息失败: failed new request: %v", err)
		return ""
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// 添加身份Cookie
	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
	response, err := httpClient.Do(request)
	if err != nil {
		log.Printf("检查身份信息失败: failed do request: %v", err)
		return ""
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Printf("检查身份信息失败: failed read response: %v", err)
		return ""
	}
	var res map[string]interface{}
	err = json.Unmarshal(body, &res)
	if err != nil {
		log.Printf("检查身份信息失败: failed unmarshal json: %v\n%v", err, string(body))
		return ""
	}
	return res["code"].(string)
}

// func checkConfig(doctorId int) bool {
// 	httpClient := &http.Client{}
// 	request, err := http.NewRequest("GET", fmt.Sprintf("https://wxis.91160.com/wxis/act/order/getYuyueConfig.do?unit_id=%v&dep_id=%v&doc_id=%v&member_id=%v",
// 		config.UnitId, config.DepId, doctorId, config.MemberId), nil)
// 	if err != nil {
// 		log.Printf("检查设置失败: failed new request: %v", err)
// 		return false
// 	}
// 	// 添加身份Cookie
// 	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
// 	response, err := httpClient.Do(request)
// 	if err != nil {
// 		log.Printf("检查设置失败: failed do request: %v", err)
// 		return false
// 	}
// 	if response.StatusCode == 200 {
// 		return true
// 	}
// 	defer response.Body.Close()
// 	body, err := ioutil.ReadAll(response.Body)
// 	if err != nil {
// 		log.Printf("检查设置失败: failed read response: %v", err)
// 		return false
// 	}
// 	log.Printf("检查设置失败: \n%v", string(body))
// 	return false
// }

// func checkBillPay() bool {
// 	httpClient := &http.Client{}
// 	request, err := http.NewRequest("POST", "https://wxis.91160.com/wxis/act/BillPayTipConfig.do", strings.NewReader("unitId="+config.UnitId))
// 	if err != nil {
// 		log.Printf("检查支付失败: failed new request: %v", err)
// 		return false
// 	}
// 	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
// 	// 添加身份Cookie
// 	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
// 	response, err := httpClient.Do(request)
// 	if err != nil {
// 		log.Printf("检查支付失败: failed do request: %v", err)
// 		return false
// 	}
// 	if response.StatusCode == 200 {
// 		return true
// 	}
// 	defer response.Body.Close()
// 	body, err := ioutil.ReadAll(response.Body)
// 	if err != nil {
// 		log.Printf("检查支付失败: failed read response: %v", err)
// 		return false
// 	}
// 	log.Printf("检查支付失败: \n%v", string(body))
// 	return false
// }
//
// func checkCertificate() bool {
// 	httpClient := &http.Client{}
// 	request, err := http.NewRequest("POST", fmt.Sprintf("https://wxis.91160.com/wxis/user/checkCertificate.do?r=%v",
// 		time.Now().UnixMicro()), strings.NewReader(fmt.Sprintf("userId=%v&config.MemberId=%v", config.UserId, config.MemberId)))
// 	if err != nil {
// 		log.Printf("检查证书失败: failed new request: %v", err)
// 		return false
// 	}
// 	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
// 	// 添加身份Cookie
// 	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
// 	response, err := httpClient.Do(request)
// 	if err != nil {
// 		log.Printf("检查证书失败: failed do request: %v", err)
// 		return false
// 	}
// 	if response.StatusCode == 200 {
// 		return true
// 	}
// 	defer response.Body.Close()
// 	body, err := ioutil.ReadAll(response.Body)
// 	if err != nil {
// 		log.Printf("检查证书失败: failed read response: %v", err)
// 		return false
// 	}
// 	log.Printf("检查证书失败: \n%v", string(body))
// 	return false
// }

// func checkRiseAmt(doctorId int) bool {
// 	httpClient := &http.Client{}
// 	request, err := http.NewRequest("POST", fmt.Sprintf("https://wxis.91160.com/wxis/act/order/checkRiseAmt.do?r=%v",
// 		time.Now().UnixMicro()), strings.NewReader(fmt.Sprintf("memberId=%v&dep_id=%v&doc_id=%v&unit_id=%v",
// 		config.MemberId, config.DepId, doctorId, config.UnitId)))
// 	if err != nil {
// 		log.Printf("检查信息失败: failed new request: %v", err)
// 		return false
// 	}
// 	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
// 	// 添加身份Cookie
// 	request.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: config.JSessionId})
// 	response, err := httpClient.Do(request)
// 	if err != nil {
// 		log.Printf("检查信息失败: failed do request: %v", err)
// 		return false
// 	}
// 	if response.StatusCode == 200 {
// 		return true
// 	}
// 	defer response.Body.Close()
// 	body, err := ioutil.ReadAll(response.Body)
// 	if err != nil {
// 		log.Printf("检查信息失败: failed read response: %v", err)
// 		return false
// 	}
// 	log.Printf("检查信息失败: \n%v", string(body))
// 	return false
// }
