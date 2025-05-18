FROM golang:1.24-bookworm

# Disable prompts on apt-get install
ENV DEBIAN_FRONTEND noninteractive

WORKDIR /app

RUN apt-get update -qq && \
	apt-get -y -q install \
		libreoffice \
		fonts-opensymbol \
		hyphen-fr \
		hyphen-de \
		hyphen-en-us \
		hyphen-it \
		hyphen-ru \
		fonts-dejavu \
		fonts-dejavu-core \
		fonts-dejavu-extra \
		fonts-droid-fallback \
		fonts-dustin \
		fonts-f500 \
		fonts-fanwood \
		fonts-freefont-ttf \
		fonts-liberation \
		fonts-lmodern \
		fonts-lyx \
		fonts-sil-gentium \
		fonts-texgyre \
		fonts-tlwg-purisa \
        ffmpeg \
        imagemagick \
	&& apt-get -y -q remove libreoffice-gnome && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*


COPY ./dockerize/imagemagic/policy.xml /etc/ImageMagick-6/policy.xml

COPY go.mod .

COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o consumer ./cmd/consumer

CMD ["./consumer"]