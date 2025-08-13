"""
Simple HTTP server to run the frontend
This allows integration between Python backend and HTML frontend
"""

import http.server
import socketserver
import webbrowser
import os
from threading import Timer

PORT = 8000


def open_browser():
    """Open browser after server starts"""
    webbrowser.open(f'http://localhost:{PORT}/frontend/')


class MyHTTPRequestHandler(http.server.SimpleHTTPRequestHandler):
    def end_headers(self):
        # Add CORS headers for future API integration
        self.send_header('Access-Control-Allow-Origin', '*')
        self.send_header('Access-Control-Allow-Methods', 'GET, POST, OPTIONS')
        super().end_headers()


if __name__ == "__main__":
    # Change to project directory
    os.chdir(os.path.dirname(os.path.abspath(__file__)))

    Handler = MyHTTPRequestHandler

    with socketserver.TCPServer(("", PORT), Handler) as httpd:
        print(f"🚀 Server running at http://localhost:{PORT}/frontend/")
        print("Press Ctrl+C to stop the server")

        # Open browser after 1 second
        Timer(1, open_browser).start()

        try:
            httpd.serve_forever()
        except KeyboardInterrupt:
            print("\n👋 Server stopped")