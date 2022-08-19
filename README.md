# 91160自动抢号
###本工具用于91160的主任医师/副主任医师号抢号预约 

###工具准备
```
Fiddler 4（抓包工具）
```

### 使用方法
1.启动抓包工具  

2.打开91160的微信版H5页面  

3.找到你想要预约科室的任意一位医师，打开到排班列表页

4.在以下接口的Cookies中获取JSESSIONID、Body中获取unit_id（医院ID）dep_id（科室ID）

```
POST https://wxis.91160.com/wxis/sch_new/schedulelist.do 
```

5.找到任意科室任意一位可以预约的医师，打开到预约确认页，选中就诊人

6.在以下接口的Params中获取member_id

```
GET https://wxis.91160.com/wxis/act/order/getYuyueConfig.do
```

7.以上参数保存至config.yaml，即可开始运行  

8.预约成功后自动结束程序，请注意91160公众号推送