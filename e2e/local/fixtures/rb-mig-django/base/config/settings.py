SECRET_KEY = "local-dev-only"
DEBUG = True
INSTALLED_APPS = ["django.contrib.contenttypes", "django.contrib.auth", "shop"]
DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.postgresql",
        "NAME": "shop",
        "USER": "shop",
        "PASSWORD": "shop",
        "HOST": "localhost",
    }
}
ROOT_URLCONF = "shop.urls"
USE_TZ = True
