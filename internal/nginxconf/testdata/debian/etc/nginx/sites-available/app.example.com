server {
	listen 443 ssl http2;
	listen [::]:443 ssl http2;
	server_name app.example.com www.app.example.com;
	ssl_certificate /etc/ssl/certs/app.pem;
	ssl_certificate_key /etc/ssl/private/app.key;
	include snippets/proxy-headers.conf;

	location / {
		proxy_pass http://api_pool;
	}
	location /static/ {
		alias /srv/app/static/;
	}
	location ~ \.php$ {
		fastcgi_pass unix:/run/php/php8.2-fpm.sock;
	}
}
server {
	listen 80;
	server_name app.example.com www.app.example.com;
	return 301 https://$host$request_uri;
}
