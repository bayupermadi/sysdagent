# sysdagent
Simple agent tool to monitor the systemd unit status changes

## Introduction
This tool used to get the status changes of the monitored service. Once changes detected it would sent alert to the recipient. Currently it support email and slack messenger to get the alert

## How to work
*sysdagent* would find the PID of monitored process that run. It monitor the changes of signal from monitored process. The signal that it monitor is SIGINT, SIGKILL, SIGTERM, SIGSTOP. 
If one of them catched by *sysdagent*, it would sent the status to receiver target which is defined in configuration file. 

## Running as backround process
*sysdagent* can be run in background process with register it as one of systemd unit. Once its run, it easier for you to fine tune the configuration and start/stop/restart this application


