#!/bin/bash

# -----------------------------------------------------------------------------
# 配置部分 - 用户需要修改此部分
# -----------------------------------------------------------------------------

setup() {
    # 设置screen会话名称（必填，默认为空防止意外运行）
    name="ai-usage"
    
    # 脚本描述
    desc="AI 用量助手：查询各 AI 提供商 API Key 用量的本地网关"
    
    # 日志设置（可选）
    enable_logging=false
    log_file="$(basename "$0" .sh).log"
    log_level=1  # 0:详细 1:信息 2:警告 3:错误
}

# 启动命令 - 用户需要实现此函数
# 返回要在screen中执行的命令字符串
start() {
    # 每个echo只能执行一个命令
    echo "${HOME}/.local/bin/aiusage serve --listen 127.0.0.1 --port 23001"
}

# 停止命令 - 用户需要实现此函数
# 返回停止应用程序的命令（通常为空，使用Ctrl+C）
stop() {
    # 默认发送Ctrl+C信号
    echo "^C"
    
    # 如果需要特殊停止命令，例如：
    # echo "pkill -f 'python app.py'"
    # echo "curl -X POST http://localhost:3000/shutdown"
}



# -----------------------------------------------------------------------------
# Screen管理函数 - 一般无需修改
# -----------------------------------------------------------------------------

# 检查screen是否存在
screen_exists() {
    screen -ls | grep -q "[0-9]*\.${name}[[:space:]]"
}

# 向screen发送命令（如果screen不存在则创建并运行命令）
send_to_screen() {
    local cmd="$*"
    
    if screen_exists; then
        screen -S "${name}" -X stuff "${cmd}"$'\n'
        log "已发送命令到 '${name}': ${cmd}" 1
    else
        screen -dmS "${name}" bash -c "${cmd}"
        log "screen会话 '${name}' 已创建并运行命令: ${cmd}" 1
    fi
}

# 启动screen并运行命令
start_screen() {
    local cmd
    cmd="$(start)"
    
    if [ -z "${cmd}" ]; then
        log "错误: start()函数未返回命令" 3
        return 1
    fi
    
    send_to_screen "${cmd}"
}

# 停止screen中的程序
stop_screen() {
    local cmd
    cmd="$(stop)"
    
    if ! service_exists; then
        log "服务未在运行，无需停止"
        
        if screen_exists; then
            screen -S "${name}" -X quit
            log "清理了空screen会话 '${name}'"
        fi
        
        return 0
    fi
    
    if [ -n "${cmd}" ]; then
        send_to_screen "${cmd}"
        wait_for_service_stop
        
        if service_exists; then
            log "警告: 发送停止命令后服务仍在运行" 2
        fi
    fi
    return 0
    
    if screen_exists; then
        screen -S "${name}" -X quit
        log "screen会话 '${name}' 已关闭"
    fi
}

# 重启
restart_screen() {
    stop_screen
    wait_for_service_stop
    start_screen
}

# 进入screen shell
attach_screen() {
    if screen_exists; then
        exec screen -r "${name}"
    else
        log "screen会话 '${name}' 不存在" 3
        return 1
    fi
}

# 从命令提取服务检测模式
extract_service_pattern() {
    local cmd="$1"
    
    if [ -z "${cmd}" ]; then
        return
    fi
    
    local last_line
    last_line="$(echo "${cmd}" | tail -1 | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
    
    if [ -n "${last_line}" ]; then
        if [[ "${last_line}" =~ ^\"(.*)\"$ ]] || [[ "${last_line}" =~ ^\'(.*)\'$ ]]; then
            last_line="${BASH_REMATCH[1]}"
        fi
        echo "${last_line}"
    fi
}

# 检查服务是否运行（内部函数）
service_exists() {
    local pattern
    pattern="$(extract_service_pattern "$(start)")"
    
    if [ -n "${pattern}" ]; then
        if ps aux | grep -v grep | grep -Fq "${pattern}"; then
            return 0
        else
            return 1
        fi
    else
        # 无法提取服务检测模式，假设服务未运行
        return 1
    fi
}

# 等待服务结束（最多10秒）
wait_for_service_stop() {
    local max_wait=10
    local count=0
    
    # 如果服务已经停止，立即返回
    if ! service_exists; then
        return 0
    fi
    
    local wait_info="等待服务停止"
    
    while [ ${count} -lt ${max_wait} ] && service_exists; do
        log "${wait_info}" 1 "flag"
        wait_info="${wait_info}."
        sleep 1
        count=$((count + 1))
    done
    
    if service_exists; then
        log "等待超时，服务仍在运行" 2
        return 1
    else
        log "服务已停止" 0
        return 0
    fi
}

# 查看状态
status_screen() {
    if screen_exists; then
        log "screen会话 '${name}' 存在"
        
        if service_exists; then
            log "服务正在运行"
        else
            log "警告: screen存在但服务未运行" 2
        fi
    else
        log "screen会话 '${name}' 不存在" 2
        
        local pattern
        pattern="$(extract_service_pattern "$(start)")"
        
        if [ -n "${pattern}" ]; then
            if ps aux | grep -v grep | grep -Fq "${pattern}"; then
                log "警告: 发现残留进程 (screen不存在但进程在运行)" 2
            fi
        fi
    fi
}


# -----------------------------------------------------------------------------
# 初始化与帮助
# -----------------------------------------------------------------------------

log() {
    local msg="$1"
    local level="${2:-1}"
    local same_line="${3}"
    
    local timestamp
    timestamp="$(date '+%Y-%m-%d %H:%M:%S')"
    local level_str
    
    case "${level}" in
        0) level_str="DEBUG" ;;
        1) level_str="INFO " ;;
        2) level_str="WARN " ;;
        3) level_str="ERROR" ;;
        *) level_str="INFO " ;;
    esac
    
    local log_msg="[${timestamp}] [${level_str}] ${msg}"

    if [ "${level}" -ge "${log_level}" ]; then
        if [ -z "${same_line}" ]; then
            echo "${log_msg}"

            if [ "${enable_logging}" = "true" ]; then
                echo "${log_msg}" >> "${log_file}"
            fi
        else
            # 回到行首模式不写入log中，只打印。
            echo -en "${log_msg}\r"
        fi
    fi
}

init() {
    cd "$(dirname "$0")" || {
        echo "错误: 无法切换到脚本目录"
        exit 1
    }
    
    setup
    
    if [ -z "${name}" ]; then
        log "错误: 未设置screen会话名称 (请在setup()中设置name变量)" 3
        exit 1
    fi
}

print_help() {
    echo "使用: $(basename "$0") [命令]"
    echo "命令:"
    echo "  start     启动应用程序"
    echo "  stop      停止应用程序"
    echo "  restart   重启应用程序"
    echo "  shell     进入screen会话"
    echo "  status    查看运行状态"
    echo "  help      显示此帮助"
    echo ""
    echo "描述: ${desc}"
}

# -----------------------------------------------------------------------------
# 主程序
# -----------------------------------------------------------------------------

main() {
    init
    
    case "${1:-help}" in
        "start")
            start_screen
            ;;
        "stop")
            stop_screen
            ;;
        "restart")
            restart_screen
            ;;
        "shell")
            attach_screen
            ;;
        "status")
            status_screen
            ;;
        "help"|"-h"|"--help")
            print_help
            ;;
        *)
            echo "未知命令: $1"
            print_help
            exit 1
            ;;
    esac
}

# 运行主程序
main "$@"
