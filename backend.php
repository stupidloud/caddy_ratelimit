<?php

// backend.php - 模拟后端认证并返回 X-Accel-* 头

// 获取请求 URI
$requestUri = $_SERVER['REQUEST_URI'];

// 只处理 /download/ 开头的请求
if (strpos($requestUri, '/download/') === 0) {
    // 模拟从请求 URI 中提取文件名
    $filename = basename($requestUri);
    if (empty($filename) || $filename === 'download') {
        $filename = 'default_file.dat'; // 如果没有文件名，使用默认值
    }

    // --- 在实际应用中，这里会进行用户认证和权限检查 ---
    $userId = 'user-for-111'; // 简单生成一个基于时间的用户 ID
    $rateLimit = 20488576; // 1 MB/s = 1024 * 1024 bytes/sec

    // 设置响应头
    header('X-Accel-Redirect: /' . $filename); // 指向内部文件路径
    header('X-Accel-User-ID: ' . $userId);
    #header('X-Accel-RateLimit: ' . $rateLimit);
    header('Content-Type: application/octet-stream'); // 可选，指示内容类型

    // 发送 200 OK 状态码，响应体为空
    http_response_code(200);
    error_log("Backend: Request for $requestUri -> Redirecting to /internal/$filename for user $userId with rate $rateLimit B/s");

} else {
    // 对于其他请求，返回 404
    http_response_code(404);
    echo "Not Found";
    error_log("Backend: Received non-download request: $requestUri");
}

?>
